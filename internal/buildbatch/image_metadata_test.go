package buildbatch

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadImageMetadataBehavior(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    imageMetadata
		wantErr string
	}{
		{name: "valid", content: `{"target":"example.com/team/image:v1"}`, want: imageMetadata{Target: "example.com/team/image:v1"}},
		{name: "trim target", content: `{"target":"  example.com/team/image:v1  "}`, want: imageMetadata{Target: "example.com/team/image:v1"}},
		{name: "invalid JSON", content: `{`, wantErr: "invalid "},
		{name: "empty target", content: `{"target":"  "}`, wantErr: " has empty target"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "metadata.json")
			if err := os.WriteFile(path, []byte(test.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := loadImageMetadata(path)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("metadata error changed: %v", err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("metadata changed: got %#v err=%v, want %#v", got, err, test.want)
			}
		})
	}
	missing := filepath.Join(t.TempDir(), "missing.json")
	if _, err := loadImageMetadata(missing); err == nil || !strings.HasPrefix(err.Error(), "read "+missing+": ") {
		t.Fatalf("metadata read error changed: %v", err)
	}
}
