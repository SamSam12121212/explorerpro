package documenthandler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"explorer/internal/citationstore"
	"explorer/internal/doccmd"
	"explorer/internal/docsplitter"
	"explorer/internal/preparedinput"
)

// ocrLine matches the per-line shape PaddleOCR emits and dococr stores.
type ocrLine struct {
	Text       string  `json:"text"`
	Bbox       []int   `json:"bbox"`
	Poly       [][]int `json:"poly,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

// ocrPage is the full per-page OCR JSON blob shape.
type ocrPage struct {
	PageWidth  int       `json:"page_width"`
	PageHeight int       `json:"page_height"`
	Lines      []ocrLine `json:"lines"`
	DurationMs int       `json:"duration_ms,omitempty"`
}

type citationStoreWriter interface {
	Create(ctx context.Context, c citationstore.Citation) (int64, error)
}

// prepareStoreCitationSpawnInput loads OCR for the requested pages, builds
// the locator child thread's first-turn input (target description + OCR
// data + optional page images), and writes it as a prepared_input blob.
// The worker side then spawns the child with tool_choice:emit_bboxes and
// one_shot=true so the child does a single response.create and releases
// its socket.
func (s *Service) prepareStoreCitationSpawnInput(ctx context.Context, req doccmd.PrepareInputRequest) (string, error) {
	doc, err := s.docs.Get(ctx, req.DocumentID)
	if err != nil {
		return "", fmt.Errorf("load document: %w", err)
	}
	if strings.TrimSpace(doc.ManifestRef) == "" {
		return "", fmt.Errorf("document has no manifest (not yet split)")
	}

	manifest, err := s.loadManifest(ctx, doc.ManifestRef)
	if err != nil {
		return "", err
	}

	pageEntries, err := selectPageEntries(manifest, req.Pages)
	if err != nil {
		return "", err
	}

	ocrPages, err := s.loadOCRForPages(ctx, pageEntries)
	if err != nil {
		return "", err
	}

	content := buildCitationLocatorContent(doc.ID, doc.Filename, pageEntries, ocrPages, req.Instruction, req.IncludeImages)

	inputJSON, err := json.Marshal([]any{
		map[string]any{
			"type":    "message",
			"role":    "user",
			"content": content,
		},
	})
	if err != nil {
		return "", fmt.Errorf("marshal citation locator input: %w", err)
	}

	store, err := preparedinput.NewStore(s.blob)
	if err != nil {
		return "", err
	}

	ref, err := store.Write(ctx, req.RequestID, preparedinput.Artifact{
		Version:    preparedinput.VersionV1,
		Input:      inputJSON,
		SourceKind: doccmd.PrepareKindStoreCitationSpawn,
		CreatedAt:  s.now().Format(time.RFC3339),
	})
	if err != nil {
		return "", err
	}
	return ref, nil
}

// prepareStoreCitationFinalizeInput parses the locator's emit_bboxes
// arguments, validates the line indices against the OCR for each page,
// materializes the bbox union + polys, persists the citation + bboxes
// via citationstore, and returns a prepared_input blob wrapping the
// {"citation_id":N} function_call_output for the parent's turn 2.
func (s *Service) prepareStoreCitationFinalizeInput(ctx context.Context, req doccmd.PrepareInputRequest) (string, error) {
	if s.citations == nil {
		return "", fmt.Errorf("citation store not configured")
	}

	doc, err := s.docs.Get(ctx, req.DocumentID)
	if err != nil {
		return "", fmt.Errorf("load document: %w", err)
	}
	if strings.TrimSpace(doc.ManifestRef) == "" {
		return "", fmt.Errorf("document has no manifest (not yet split)")
	}

	manifest, err := s.loadManifest(ctx, doc.ManifestRef)
	if err != nil {
		return "", err
	}

	pageEntries, err := selectPageEntries(manifest, req.Pages)
	if err != nil {
		return "", err
	}

	ocrPages, err := s.loadOCRForPages(ctx, pageEntries)
	if err != nil {
		return "", err
	}

	locatorOutput, err := decodeEmitBboxesArguments(req.ToolCallArgsJSON)
	if err != nil {
		return "", err
	}

	bboxes, err := materializeCitationBboxes(locatorOutput, ocrPages, req.Pages)
	if err != nil {
		return "", err
	}

	citationID, err := s.citations.Create(ctx, citationstore.Citation{
		RootThreadID: req.ThreadID,
		DocumentID:   req.DocumentID,
		Instruction:  req.Instruction,
		Bboxes:       bboxes,
	})
	if err != nil {
		return "", fmt.Errorf("persist citation: %w", err)
	}

	outputStr, err := json.Marshal(map[string]any{"citation_id": citationID})
	if err != nil {
		return "", fmt.Errorf("marshal citation output: %w", err)
	}

	inputJSON, err := json.Marshal([]any{
		map[string]any{
			"type":    "function_call_output",
			"call_id": req.CallID,
			"output":  string(outputStr),
		},
	})
	if err != nil {
		return "", fmt.Errorf("marshal finalize input: %w", err)
	}

	store, err := preparedinput.NewStore(s.blob)
	if err != nil {
		return "", err
	}

	ref, err := store.Write(ctx, req.RequestID, preparedinput.Artifact{
		Version:    preparedinput.VersionV1,
		Input:      inputJSON,
		SourceKind: doccmd.PrepareKindStoreCitationFinalize,
		CreatedAt:  s.now().Format(time.RFC3339),
	})
	if err != nil {
		return "", err
	}

	s.logger.Info("citation persisted",
		"citation_id", citationID,
		"root_thread_id", req.ThreadID,
		"document_id", req.DocumentID,
		"pages", req.Pages,
		"child_thread_id", req.ChildThreadID,
	)
	return ref, nil
}

// selectPageEntries resolves the requested 1-indexed page numbers against
// the manifest, returning entries in the same order. Missing pages error
// out — the model is asking for evidence on a page that doesn't exist.
func selectPageEntries(manifest docsplitter.Manifest, pages []int) ([]docsplitter.PageEntry, error) {
	byNumber := make(map[int]docsplitter.PageEntry, len(manifest.Pages))
	for _, p := range manifest.Pages {
		byNumber[p.PageNumber] = p
	}

	out := make([]docsplitter.PageEntry, 0, len(pages))
	for _, pageNumber := range pages {
		entry, ok := byNumber[pageNumber]
		if !ok {
			return nil, fmt.Errorf("page %d not found in manifest (page_count=%d)", pageNumber, manifest.PageCount)
		}
		out = append(out, entry)
	}
	return out, nil
}

// loadOCRForPages returns the parsed OCR payload per page. Every page
// must have an ocr_ref — the locator cannot work without OCR data.
func (s *Service) loadOCRForPages(ctx context.Context, pages []docsplitter.PageEntry) (map[int]ocrPage, error) {
	out := make(map[int]ocrPage, len(pages))
	for _, p := range pages {
		if strings.TrimSpace(p.OCRRef) == "" {
			return nil, fmt.Errorf("page %d has no ocr_ref (OCR not run yet)", p.PageNumber)
		}
		raw, err := s.blob.ReadRef(ctx, p.OCRRef)
		if err != nil {
			return nil, fmt.Errorf("read ocr blob for page %d: %w", p.PageNumber, err)
		}
		var parsed ocrPage
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, fmt.Errorf("decode ocr json for page %d: %w", p.PageNumber, err)
		}
		out[p.PageNumber] = parsed
	}
	return out, nil
}

// buildCitationLocatorContent constructs the user message content array
// for the locator child thread. Shape is an <envelope> of XML-ish tags
// so the locator can parse OCR without confusing it for caller text.
// Images are attached only when include_images is true (caller's call —
// based on scan quality).
func buildCitationLocatorContent(documentID int64, filename string, pages []docsplitter.PageEntry, ocrPages map[int]ocrPage, instruction string, includeImages bool) []any {
	content := make([]any, 0, len(pages)*3+4)

	header := fmt.Sprintf(`<target document_id="%d" filename=%q>
%s
</target>`, documentID, filename, strings.TrimSpace(instruction))
	content = append(content, map[string]any{
		"type": "input_text",
		"text": header,
	})

	for _, page := range pages {
		data := ocrPages[page.PageNumber]
		var linesBuilder strings.Builder
		for index, line := range data.Lines {
			if len(line.Bbox) >= 4 {
				// bbox is [x0, y0, x1, y1] in manifest pixels, top-left
				// origin. Gives the locator layout signal for tables,
				// forms, and multi-column prose where text order alone
				// isn't enough to tell which lines belong together.
				// Poly is omitted — bbox is enough for row/label
				// grouping, polys double the token cost for little gain.
				fmt.Fprintf(&linesBuilder, "[%d] bbox=[%d,%d,%d,%d] %s\n",
					index, line.Bbox[0], line.Bbox[1], line.Bbox[2], line.Bbox[3], line.Text)
			} else {
				fmt.Fprintf(&linesBuilder, "[%d] %s\n", index, line.Text)
			}
		}

		pageBlock := fmt.Sprintf(`<page number="%d" width="%d" height="%d">
<ocr_lines>
%s</ocr_lines>
</page>`, page.PageNumber, page.Width, page.Height, linesBuilder.String())

		content = append(content, map[string]any{
			"type": "input_text",
			"text": pageBlock,
		})

		if includeImages && strings.TrimSpace(page.ImageRef) != "" {
			image := map[string]any{
				"type":      "image_ref",
				"image_ref": page.ImageRef,
				"detail":    "high",
			}
			if strings.TrimSpace(page.ContentType) != "" {
				image["content_type"] = page.ContentType
			}
			content = append(content, image)
		}
	}

	return content
}

// emitBboxesArguments matches the structured shape the locator's forced
// tool call produces.
type emitBboxesArguments struct {
	Pages []emitBboxesPage `json:"pages"`
}

type emitBboxesPage struct {
	Page        int   `json:"page"`
	LineIndices []int `json:"line_indices"`
}

func decodeEmitBboxesArguments(raw string) (emitBboxesArguments, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return emitBboxesArguments{}, fmt.Errorf("emit_bboxes arguments are empty")
	}
	var args emitBboxesArguments
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return emitBboxesArguments{}, fmt.Errorf("decode emit_bboxes arguments: %w", err)
	}
	if len(args.Pages) == 0 {
		return emitBboxesArguments{}, fmt.Errorf("emit_bboxes returned no pages")
	}
	return args, nil
}

// materializeCitationBboxes validates the locator's line indices against
// the OCR for each page (structural hallucination gate: the model can
// only return indices that exist) and emits one bbox row per OCR line.
//
// Earlier versions produced a single unioned envelope per page, which
// rendered as one giant rectangle that swallowed the whitespace between
// a heading and its paragraphs. Per-line rows paint as stacked text-
// sized highlights — the standard marker look.
func materializeCitationBboxes(output emitBboxesArguments, ocrPages map[int]ocrPage, allowedPages []int) ([]citationstore.Bbox, error) {
	allowed := make(map[int]bool, len(allowedPages))
	for _, p := range allowedPages {
		allowed[p] = true
	}

	out := make([]citationstore.Bbox, 0, len(output.Pages))
	for _, page := range output.Pages {
		if !allowed[page.Page] {
			return nil, fmt.Errorf("locator returned page %d which was not in the request (allowed=%v)", page.Page, allowedPages)
		}
		data, ok := ocrPages[page.Page]
		if !ok {
			return nil, fmt.Errorf("locator returned page %d but OCR data was not loaded for it", page.Page)
		}
		if len(page.LineIndices) == 0 {
			return nil, fmt.Errorf("locator returned empty line_indices for page %d", page.Page)
		}

		for _, idx := range page.LineIndices {
			if idx < 0 || idx >= len(data.Lines) {
				return nil, fmt.Errorf("locator returned line index %d for page %d but only %d lines exist", idx, page.Page, len(data.Lines))
			}
			line := data.Lines[idx]
			if len(line.Bbox) < 4 {
				return nil, fmt.Errorf("page %d line %d has malformed bbox (len=%d)", page.Page, idx, len(line.Bbox))
			}

			var polyJSON json.RawMessage
			if len(line.Poly) > 0 {
				raw, err := json.Marshal(line.Poly)
				if err != nil {
					return nil, fmt.Errorf("marshal poly for page %d line %d: %w", page.Page, idx, err)
				}
				polyJSON = raw
			}

			out = append(out, citationstore.Bbox{
				Page:        page.Page,
				LineIndices: []int32{int32(idx)},
				X:           line.Bbox[0],
				Y:           line.Bbox[1],
				Width:       line.Bbox[2] - line.Bbox[0],
				Height:      line.Bbox[3] - line.Bbox[1],
				PolyJSON:    polyJSON,
			})
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("locator produced no usable bboxes")
	}
	return out, nil
}
