"""PaddleOCR HTTP service for the Explorer evidence-chain pipeline.

POST /ocr        text detection + recognition, returns lines with bboxes in page pixels
POST /structure  PP-StructureV3 layout analysis, returns typed blocks
GET  /health     liveness
"""
from __future__ import annotations

import asyncio
import hmac
import io
import logging
import os
import tempfile
import threading
import time
from pathlib import Path
from typing import Annotated

import numpy as np
from fastapi import FastAPI, File, Header, HTTPException, UploadFile
from fastapi.responses import Response
from PIL import Image
from pydantic import BaseModel

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s %(message)s",
)
log = logging.getLogger("paddleocr-service")


AUTH_TOKEN = os.environ.get("PADDLEOCR_BEARER_TOKEN", "").strip()
LANG = (os.environ.get("PADDLEOCR_LANG", "en").strip() or "en")

if not AUTH_TOKEN:
    log.warning("PADDLEOCR_BEARER_TOKEN is empty — endpoints will accept unauthenticated requests")

log.info("Importing PaddleOCR (lang=%s)…", LANG)
from paddleocr import PaddleOCR  # noqa: E402

log.info("Instantiating PaddleOCR (downloads weights on first run)…")
_ocr = PaddleOCR(use_textline_orientation=True, lang=LANG)
log.info("PaddleOCR ready.")

# PaddleOCR.predict is not thread-safe — concurrent calls on the same
# instance crash with RuntimeError: std::exception (see
# https://github.com/PaddlePaddle/PaddleOCR/issues/16238). A single
# async semaphore serializes every inference call across all endpoints;
# we have one GPU anyway, so parallelism was illusory. The await on
# .acquire() yields to the event loop so /health stays responsive.
_inference_sem = asyncio.Semaphore(1)


class Line(BaseModel):
    text: str
    bbox: list[int]
    poly: list[list[int]]
    confidence: float


class OCRResponse(BaseModel):
    page_width: int
    page_height: int
    lines: list[Line]
    duration_ms: int


class StructureBlock(BaseModel):
    type: str
    bbox: list[int]
    text: str | None = None


class StructureResponse(BaseModel):
    page_width: int
    page_height: int
    blocks: list[StructureBlock]
    duration_ms: int


def _check_auth(authorization: str | None) -> None:
    if not AUTH_TOKEN:
        return
    if not authorization:
        raise HTTPException(status_code=401, detail="missing or malformed authorization header")
    scheme, _, token = authorization.partition(" ")
    token = token.strip()
    if scheme.lower() != "bearer" or not token:
        raise HTTPException(status_code=401, detail="missing or malformed authorization header")
    if not hmac.compare_digest(token, AUTH_TOKEN):
        raise HTTPException(status_code=401, detail="invalid bearer token")


def _poly_to_bbox(poly: list[list[int]]) -> list[int]:
    xs = [p[0] for p in poly]
    ys = [p[1] for p in poly]
    return [min(xs), min(ys), max(xs), max(ys)]


def _load_image(data: bytes) -> tuple[np.ndarray, int, int]:
    if not data:
        raise HTTPException(status_code=400, detail="empty image body")
    try:
        pil = Image.open(io.BytesIO(data)).convert("RGB")
    except Exception as err:
        raise HTTPException(status_code=400, detail=f"could not decode image: {err}") from err
    arr = np.array(pil)
    width, height = pil.size
    return arr, width, height


def _get_field(obj, key, default):
    if isinstance(obj, dict):
        return obj.get(key, default)
    return getattr(obj, key, default)


app = FastAPI(title="PaddleOCR Service", version="1.0.0")


@app.get("/health")
def health() -> dict:
    return {"status": "ok", "lang": LANG}


@app.post("/ocr", response_model=OCRResponse)
async def ocr_endpoint(
    image: Annotated[UploadFile, File(description="page image (PNG/JPEG)")],
    authorization: Annotated[str | None, Header()] = None,
) -> OCRResponse:
    _check_auth(authorization)
    t0 = time.perf_counter()
    arr, width, height = _load_image(await image.read())

    # Inference is blocking + CPU/GPU-bound; run off the event loop so /health
    # stays responsive. Semaphore(1) serializes concurrent predict calls —
    # PaddleOCR.predict is not thread-safe.
    async with _inference_sem:
        results = await asyncio.to_thread(_ocr.predict, arr)
    lines: list[Line] = []

    if results:
        first = results[0]
        texts = _get_field(first, "rec_texts", [])
        polys = _get_field(first, "rec_polys", [])
        scores = _get_field(first, "rec_scores", [])

        for text, poly, score in zip(texts, polys, scores):
            poly_ints = [[int(round(float(p[0]))), int(round(float(p[1])))] for p in poly]
            lines.append(
                Line(
                    text=str(text),
                    poly=poly_ints,
                    bbox=_poly_to_bbox(poly_ints),
                    confidence=float(score),
                )
            )

    duration = int((time.perf_counter() - t0) * 1000)
    log.info("/ocr image=%dx%d lines=%d duration_ms=%d", width, height, len(lines), duration)
    return OCRResponse(page_width=width, page_height=height, lines=lines, duration_ms=duration)


@app.post("/ocr/visualize")
async def ocr_visualize(
    image: Annotated[UploadFile, File(description="page image (PNG/JPEG)")],
    authorization: Annotated[str | None, Header()] = None,
) -> Response:
    """Return an annotated image with detection polygons and recognized text drawn on it."""
    _check_auth(authorization)
    arr, _w, _h = _load_image(await image.read())

    with tempfile.TemporaryDirectory() as tmp:
        tmp_path = Path(tmp)
        # Keep predict + save_to_img atomic under one semaphore acquisition.
        # save_to_img reads state populated by predict; a concurrent request
        # acquiring the semaphore between them would mutate that state and
        # corrupt the visualization. File I/O below runs outside the lock.
        async with _inference_sem:
            results = await asyncio.to_thread(_ocr.predict, arr)
            if not results:
                raise HTTPException(status_code=404, detail="no OCR results")
            await asyncio.to_thread(results[0].save_to_img, str(tmp_path))
        candidates = sorted([p for p in tmp_path.rglob("*") if p.suffix.lower() in {".png", ".jpg", ".jpeg"}])
        if not candidates:
            raise HTTPException(status_code=500, detail="save_to_img produced no output file")
        data = candidates[0].read_bytes()
        media = "image/png" if candidates[0].suffix.lower() == ".png" else "image/jpeg"

    return Response(content=data, media_type=media)


_structure = None
_structure_lock = threading.Lock()


def _get_structure():
    # Double-checked locking: fast path with no lock once initialized,
    # lock-guarded init on first call. Without the lock two concurrent
    # /structure requests could both see `_structure is None` in
    # separate threadpool threads and try to load the ~800MB
    # PPStructureV3 models twice — wastes time, doubles VRAM, risks OOM.
    global _structure
    if _structure is not None:
        return _structure
    with _structure_lock:
        if _structure is None:
            log.info("Loading PP-StructureV3…")
            from paddleocr import PPStructureV3
            _structure = PPStructureV3()
            log.info("PP-StructureV3 ready.")
        return _structure


@app.post("/structure", response_model=StructureResponse)
async def structure_endpoint(
    image: Annotated[UploadFile, File(description="page image (PNG/JPEG)")],
    authorization: Annotated[str | None, Header()] = None,
) -> StructureResponse:
    _check_auth(authorization)
    t0 = time.perf_counter()
    arr, width, height = _load_image(await image.read())

    # First call triggers ~60-90s lazy model load — absolutely must not
    # block the event loop, or /health goes dark for the whole duration.
    # The load itself is protected by threading.Lock inside _get_structure
    # (see double-check). Inference runs under the inference semaphore.
    pipeline = await asyncio.to_thread(_get_structure)
    async with _inference_sem:
        results = await asyncio.to_thread(pipeline.predict, arr)

    blocks: list[StructureBlock] = []
    if results:
        first = results[0]
        layout_res = _get_field(first, "layout_det_res", {}) or {}
        boxes = _get_field(layout_res, "boxes", []) or []
        for item in boxes:
            coord = _get_field(item, "coordinate", None) or _get_field(item, "bbox", None) or []
            if len(coord) != 4:
                continue
            label = _get_field(item, "label", None) or _get_field(item, "type", None) or "unknown"
            blocks.append(
                StructureBlock(
                    type=str(label),
                    bbox=[int(round(float(v))) for v in coord],
                )
            )

    duration = int((time.perf_counter() - t0) * 1000)
    log.info("/structure image=%dx%d blocks=%d duration_ms=%d", width, height, len(blocks), duration)
    return StructureResponse(page_width=width, page_height=height, blocks=blocks, duration_ms=duration)
