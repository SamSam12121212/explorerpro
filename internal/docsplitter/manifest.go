package docsplitter

type Manifest struct {
	Version              string       `json:"version"`
	DocumentID           string       `json:"document_id"`
	CreatedAt            string       `json:"created_at"`
	PageCount            int          `json:"page_count"`
	AssetsRootRef        string       `json:"assets_root_ref"`
	RenderBackend        string       `json:"render_backend"`
	RenderBackendVersion string       `json:"render_backend_version"`
	RenderParams         RenderParams `json:"render_params"`
	Pages                []PageEntry  `json:"pages"`
}

type RenderParams struct {
	Format string `json:"format"`
	DPI    int    `json:"dpi"`
}

type PageEntry struct {
	PageNumber  int    `json:"page_number"`
	ImageRef    string `json:"image_ref"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	ContentType string `json:"content_type"`
	SHA256      string `json:"sha256"`
}
