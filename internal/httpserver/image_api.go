package httpserver

import (
	"bytes"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"explorer/internal/idgen"
	"explorer/internal/ocrcmd"

	"github.com/nats-io/nats.go"
)

const maxImageUploadBytes = 20 << 20

func (a *commandAPI) handleImageServe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}

	subPath := strings.TrimPrefix(r.URL.Path, "/")
	ref := "blob://" + subPath

	data, err := a.runtime.Blob().ReadRef(r.Context(), ref)
	if err != nil {
		writeErrorJSON(w, http.StatusNotFound, "image not found")
		return
	}

	contentType := http.DetectContentType(data)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (a *commandAPI) handleImages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}

	if err := r.ParseMultipartForm(maxImageUploadBytes); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, fmt.Sprintf("parse multipart form: %v", err))
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "file field is required")
		return
	}
	defer file.Close()

	payload := bytes.Buffer{}
	limited := http.MaxBytesReader(w, file, maxImageUploadBytes+1)
	if _, err := payload.ReadFrom(limited); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, fmt.Sprintf("read image upload: %v", err))
		return
	}
	if payload.Len() == 0 {
		writeErrorJSON(w, http.StatusBadRequest, "uploaded image is empty")
		return
	}
	if payload.Len() > maxImageUploadBytes {
		writeErrorJSON(w, http.StatusBadRequest, fmt.Sprintf("uploaded image exceeds %d bytes", maxImageUploadBytes))
		return
	}

	contentType := http.DetectContentType(payload.Bytes())
	if !strings.HasPrefix(contentType, "image/") {
		writeErrorJSON(w, http.StatusBadRequest, fmt.Sprintf("uploaded file is not an image (detected %s)", contentType))
		return
	}

	imageID, err := idgen.New("img")
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	filename := strings.TrimSpace(header.Filename)
	extension := filepath.Ext(filename)
	if extension == "" {
		if extensions, err := mime.ExtensionsByType(contentType); err == nil && len(extensions) > 0 {
			extension = extensions[0]
		}
	}
	if extension == "" {
		extension = ".bin"
	}

	ref := a.runtime.Blob().Ref("images", imageID, "source"+extension)
	if err := a.runtime.Blob().WriteRef(r.Context(), ref, payload.Bytes()); err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("store uploaded image: %v", err))
		return
	}

	if err := a.publishImageOCRRequested(imageID, ref, contentType); err != nil {
		writeErrorJSON(w, http.StatusServiceUnavailable, fmt.Sprintf("request image ocr: %v", err))
		return
	}

	image := map[string]any{
		"image_id":     imageID,
		"image_ref":    ref,
		"content_type": contentType,
		"bytes":        payload.Len(),
	}
	if filename != "" {
		image["filename"] = filename
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"image": image,
	})
}

func (a *commandAPI) publishImageOCRRequested(imageID, imageRef, contentType string) error {
	payload, err := ocrcmd.EncodeImageOCRRequested(ocrcmd.ImageOCRRequestedEvent{
		ImageID:     imageID,
		ImageRef:    imageRef,
		ContentType: contentType,
	})
	if err != nil {
		return fmt.Errorf("encode image ocr requested event: %w", err)
	}

	msg := &nats.Msg{
		Subject: ocrcmd.ImageOCRRequestedSubject,
		Header:  nats.Header{},
		Data:    payload,
	}
	msg.Header.Set("Nats-Msg-Id", imageID)

	if _, err := a.runtime.NATS().JetStream().PublishMsg(msg); err != nil {
		return fmt.Errorf("publish %s: %w", ocrcmd.ImageOCRRequestedSubject, err)
	}
	return nil
}
