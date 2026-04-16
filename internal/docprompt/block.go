// Package docprompt formats the <available_documents> and <collection>
// blocks that are appended to the model's instructions at runtime.
//
// Two callers produce these blocks: the documenthandler service (primary
// path, reached via NATS) and the worker actor's local fallback (used when
// the documenthandler is unreachable). Keeping the formatting here — not
// duplicated across both — means any change to XML shape, escaping, or
// empty-collection handling lands in one place.
package docprompt

import (
	"strconv"
	"strings"

	"explorer/internal/docstore"
	"explorer/internal/threadcollectionstore"
)

// AppendAvailableDocumentsBlock joins the given documents + collections block
// onto base with a blank-line separator, returning base unchanged when the
// block is empty.
func AppendAvailableDocumentsBlock(
	base string,
	documents []docstore.Document,
	collections []threadcollectionstore.AttachedCollection,
) string {
	block := FormatAvailableDocumentsBlock(documents, collections)
	if block == "" {
		return base
	}

	trimmedBase := strings.TrimRight(base, "\n")
	if strings.TrimSpace(trimmedBase) == "" {
		return block
	}

	return trimmedBase + "\n\n" + block
}

// FormatAvailableDocumentsBlock renders the full runtime-context block:
// a top-level <available_documents> wrapper for standalone documents (if any),
// followed by one <collection name="..."> wrapper per attached collection.
// Returns "" when both inputs are empty.
func FormatAvailableDocumentsBlock(
	documents []docstore.Document,
	collections []threadcollectionstore.AttachedCollection,
) string {
	var parts []string

	if standalone := FormatDocumentsBlock(documents, "<available_documents>", "</available_documents>"); standalone != "" {
		parts = append(parts, standalone)
	}

	for _, collection := range collections {
		parts = append(parts, FormatCollectionBlock(collection))
	}

	return strings.Join(parts, "\n")
}

// FormatDocumentsBlock renders the given documents wrapped in openTag/closeTag.
// Documents with non-positive IDs are skipped. Returns "" when no document is
// eligible — callers use that to decide whether to emit the wrapper at all.
func FormatDocumentsBlock(documents []docstore.Document, openTag, closeTag string) string {
	var builder strings.Builder
	count := 0

	for _, document := range documents {
		if document.ID <= 0 {
			continue
		}
		id := formatDocumentID(document.ID)

		name := strings.TrimSpace(document.Filename)
		if name == "" {
			name = id
		}

		if count == 0 {
			builder.WriteString(openTag)
			builder.WriteByte('\n')
		}
		builder.WriteString(`<document id="`)
		builder.WriteString(escapeAttribute(id))
		builder.WriteString(`" name="`)
		builder.WriteString(escapeAttribute(name))
		builder.WriteString(`" />`)
		builder.WriteByte('\n')
		count++
	}

	if count == 0 {
		return ""
	}

	builder.WriteString(closeTag)
	return builder.String()
}

// FormatCollectionBlock renders a single collection as
// <collection name="..."><available_documents>...</available_documents></collection>.
// Empty collections still emit the wrapper so the model knows the collection
// is attached (members can appear mid-thread).
func FormatCollectionBlock(collection threadcollectionstore.AttachedCollection) string {
	name := strings.TrimSpace(collection.Name)
	if name == "" {
		name = collection.ID
	}

	var builder strings.Builder
	builder.WriteString(`<collection name="`)
	builder.WriteString(escapeAttribute(name))
	builder.WriteString(`">` + "\n")
	if inner := FormatDocumentsBlock(collection.Documents, "<available_documents>", "</available_documents>"); inner != "" {
		builder.WriteString(inner)
		builder.WriteByte('\n')
	} else {
		builder.WriteString("<available_documents></available_documents>\n")
	}
	builder.WriteString("</collection>")
	return builder.String()
}

// EscapeAttribute XML-escapes a value for use inside a double-quoted attribute.
// Exported so callers that assemble their own tags (e.g. tests) can match
// the same escaping rules.
func EscapeAttribute(value string) string {
	return escapeAttribute(value)
}

func escapeAttribute(value string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		`"`, "&quot;",
		"<", "&lt;",
		">", "&gt;",
		"\n", "&#10;",
		"\r", "&#13;",
		"\t", "&#9;",
	)
	return replacer.Replace(value)
}

func formatDocumentID(id int64) string {
	return strconv.FormatInt(id, 10)
}
