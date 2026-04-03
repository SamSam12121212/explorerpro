package httpserver

import (
	"bytes"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"explorer/internal/idgen"
)

const maxImageUploadBytes = 20 << 20

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
