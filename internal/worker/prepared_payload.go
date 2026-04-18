package worker

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"explorer/internal/blobstore"
	"explorer/internal/preparedinput"
)

func materializePreparedInputPayloadWithBlob(ctx context.Context, blob *blobstore.LocalStore, payload map[string]any) (map[string]any, error) {
	sendPayload, ok := cloneAny(payload).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("response.create payload must be an object")
	}

	ref := strings.TrimSpace(stringValue(sendPayload["prepared_input_ref"]))
	if ref == "" {
		return sendPayload, nil
	}

	inputValue, err := loadPreparedInputValueFromBlob(ctx, blob, ref)
	if err != nil {
		return nil, err
	}

	sendPayload["input"] = inputValue
	delete(sendPayload, "prepared_input_ref")
	return sendPayload, nil
}

func loadPreparedInputValueFromBlob(ctx context.Context, blob *blobstore.LocalStore, ref string) (any, error) {
	if blob == nil {
		return nil, fmt.Errorf("blob store is not configured")
	}

	store, err := preparedinput.NewStore(blob)
	if err != nil {
		return nil, err
	}

	artifact, err := store.Read(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("load prepared input %q: %w", ref, err)
	}

	normalized, err := normalizeInputItems(artifact.Input)
	if err != nil {
		return nil, fmt.Errorf("normalize prepared input %q: %w", ref, err)
	}

	value, err := decodeResponseCreateField("input", normalized)
	if err != nil {
		return nil, fmt.Errorf("decode prepared input %q: %w", ref, err)
	}

	return value, nil
}

func lowerResponseCreatePayloadWithBlob(ctx context.Context, blob *blobstore.LocalStore, payload map[string]any) (map[string]any, payloadLoweringStats, error) {
	cloned := cloneAny(payload)
	root, ok := cloned.(map[string]any)
	if !ok {
		return nil, payloadLoweringStats{}, fmt.Errorf("response.create payload must be an object")
	}

	stats := payloadLoweringStats{}
	input, ok := root["input"]
	if !ok {
		return root, stats, nil
	}

	lowered, err := lowerInputValueWithBlob(ctx, blob, input, &stats)
	if err != nil {
		return nil, payloadLoweringStats{}, err
	}
	root["input"] = lowered
	return root, stats, nil
}

func lowerInputValueWithBlob(ctx context.Context, blob *blobstore.LocalStore, value any, stats *payloadLoweringStats) (any, error) {
	items, ok := value.([]any)
	if !ok {
		return value, nil
	}
	stats.InputItemsCount = len(items)

	lowered := make([]any, len(items))
	for index, item := range items {
		itemMap, ok := item.(map[string]any)
		if !ok {
			lowered[index] = item
			continue
		}

		loweredItem, err := lowerInputItemWithBlob(ctx, blob, itemMap, stats)
		if err != nil {
			return nil, err
		}
		lowered[index] = loweredItem
	}

	return lowered, nil
}

func lowerInputItemWithBlob(ctx context.Context, blob *blobstore.LocalStore, item map[string]any, stats *payloadLoweringStats) (map[string]any, error) {
	switch item["type"] {
	case "message":
		content, ok := item["content"].([]any)
		if !ok {
			return item, nil
		}
		lowered, err := lowerContentPartsWithBlob(ctx, blob, content, stats)
		if err != nil {
			return nil, err
		}
		item["content"] = lowered
	case "function_call_output":
		// output may be a string (text-only tool result) or an array of
		// input_text / input_image / input_file parts. Only the array form
		// can carry blob refs that need lowering.
		output, ok := item["output"].([]any)
		if !ok {
			return item, nil
		}
		lowered, err := lowerContentPartsWithBlob(ctx, blob, output, stats)
		if err != nil {
			return nil, err
		}
		item["output"] = lowered
	}
	return item, nil
}

func lowerContentPartsWithBlob(ctx context.Context, blob *blobstore.LocalStore, parts []any, stats *payloadLoweringStats) ([]any, error) {
	lowered := make([]any, len(parts))
	for index, part := range parts {
		partMap, ok := part.(map[string]any)
		if !ok {
			lowered[index] = part
			continue
		}
		loweredPart, err := lowerMessageContentItemWithBlob(ctx, blob, partMap, stats)
		if err != nil {
			return nil, err
		}
		lowered[index] = loweredPart
	}
	return lowered, nil
}

func lowerMessageContentItemWithBlob(ctx context.Context, blob *blobstore.LocalStore, item map[string]any, stats *payloadLoweringStats) (map[string]any, error) {
	typeName := stringValue(item["type"])
	switch typeName {
	case "image_ref":
		stats.LoweredImageInputs++
		stats.LoweredBlobRefs++
		return buildInputImageFromBlobRefWithBlob(ctx, blob, item, stringValue(item["image_ref"]))
	case "input_image":
		imageURL := stringValue(item["image_url"])
		if strings.HasPrefix(imageURL, "blob://") {
			stats.LoweredImageInputs++
			stats.LoweredBlobRefs++
			return buildInputImageFromBlobRefWithBlob(ctx, blob, item, imageURL)
		}
	}

	return item, nil
}

func buildInputImageFromBlobRefWithBlob(ctx context.Context, blob *blobstore.LocalStore, item map[string]any, ref string) (map[string]any, error) {
	if blob == nil {
		return nil, fmt.Errorf("blob store is not configured")
	}
	if strings.TrimSpace(ref) == "" {
		return nil, fmt.Errorf("image input is missing image_ref")
	}

	data, err := blob.ReadRef(ctx, ref)
	if err != nil {
		return nil, err
	}

	contentType := strings.TrimSpace(stringValue(item["content_type"]))
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	if !strings.HasPrefix(contentType, "image/") {
		return nil, fmt.Errorf("blob ref %s is not an image (detected %s)", ref, contentType)
	}

	detail := strings.TrimSpace(stringValue(item["detail"]))
	if detail == "" {
		detail = "auto"
	}

	dataURL := "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data)
	return map[string]any{
		"type":      "input_image",
		"image_url": dataURL,
		"detail":    detail,
	}, nil
}
