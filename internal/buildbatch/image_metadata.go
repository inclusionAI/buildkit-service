package buildbatch

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type imageMetadata struct {
	Target string `json:"target"`
}

func loadImageMetadata(metaPath string) (imageMetadata, error) {
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return imageMetadata{}, fmt.Errorf("read %s: %w", metaPath, err)
	}
	var meta imageMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return imageMetadata{}, fmt.Errorf("invalid %s: %w", metaPath, err)
	}
	meta.Target = strings.TrimSpace(meta.Target)
	if meta.Target == "" {
		return imageMetadata{}, fmt.Errorf("%s has empty target", metaPath)
	}
	return meta, nil
}
