package dockerfilepreprocess

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTransformLegacyHeredocsEpisodeExample(t *testing.T) {
	input := `FROM ubuntu:22.04

# Create a project README that mentions the primary download URL
RUN cat > /workspace/README.md << 'READMEEOF'
# Episode Processing Project

## Source
Download episode videos from the media archive server:
http://media-archive.bcorp.local/episodes/

## Notes
- Episode files are large MP4 files (H.264 video, AAC audio)
- Network issues have been reported with the media archive server
- A local cache of previously downloaded episodes may be available
READMEEOF

# Create a download script that references the blocked URL
RUN cat > /workspace/download_episode.sh << 'DLEOF'
#!/bin/bash
# Download episode video from the media archive
EPISODE_URL="http://media-archive.bcorp.local/episodes/episode42.mp4"
OUTPUT="/workspace/episode42.mp4"

echo "Downloading episode from ${EPISODE_URL}..."
curl -f -L -o "${OUTPUT}" "${EPISODE_URL}"
if [ $? -ne 0 ]; then
    echo "ERROR: Download failed from media archive."
    echo "Check network connectivity or use a cached version if available."
    exit 1
fi
echo "Download complete: ${OUTPUT}"
DLEOF
RUN chmod +x /workspace/download_episode.sh

# Create a cache README
RUN cat > /workspace/cache/README.txt << 'CACEOF'
Local Cache
===========
This directory contains cached copies of media files for offline use.
Files in this cache are identical to the originals on the media server.
CACEOF
`

	want := `FROM ubuntu:22.04

# Create a project README that mentions the primary download URL
COPY <<'READMEEOF' /workspace/README.md
# Episode Processing Project

## Source
Download episode videos from the media archive server:
http://media-archive.bcorp.local/episodes/

## Notes
- Episode files are large MP4 files (H.264 video, AAC audio)
- Network issues have been reported with the media archive server
- A local cache of previously downloaded episodes may be available
READMEEOF

# Create a download script that references the blocked URL
COPY <<'DLEOF' /workspace/download_episode.sh
#!/bin/bash
# Download episode video from the media archive
EPISODE_URL="http://media-archive.bcorp.local/episodes/episode42.mp4"
OUTPUT="/workspace/episode42.mp4"

echo "Downloading episode from ${EPISODE_URL}..."
curl -f -L -o "${OUTPUT}" "${EPISODE_URL}"
if [ $? -ne 0 ]; then
    echo "ERROR: Download failed from media archive."
    echo "Check network connectivity or use a cached version if available."
    exit 1
fi
echo "Download complete: ${OUTPUT}"
DLEOF
RUN chmod +x /workspace/download_episode.sh

# Create a cache README
COPY <<'CACEOF' /workspace/cache/README.txt
Local Cache
===========
This directory contains cached copies of media files for offline use.
Files in this cache are identical to the originals on the media server.
CACEOF
`

	got, changed, err := TransformLegacyHeredocs([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected Dockerfile to be rewritten")
	}
	if string(got) != want {
		t.Fatalf("unexpected rewrite:\n%s", string(got))
	}
	if strings.Contains(string(got), "RUN cat >") {
		t.Fatalf("legacy heredoc RUN remains after rewrite:\n%s", string(got))
	}
}

func TestTransformLegacyHeredocsCatBeforeRedirect(t *testing.T) {
	input := "FROM alpine\nRUN cat <<EOF > script.sh\necho ok\nEOF\n"
	want := "FROM alpine\nCOPY <<EOF script.sh\necho ok\nEOF\n"
	got, changed, err := TransformLegacyHeredocs([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if !changed || string(got) != want {
		t.Fatalf("changed=%v got:\n%s", changed, got)
	}
}

func TestTransformLegacyHeredocsRejectsAppend(t *testing.T) {
	_, _, err := TransformLegacyHeredocs([]byte("FROM alpine\nRUN cat >> script.sh <<EOF\necho ok\nEOF\n"))
	if err == nil {
		t.Fatal("expected append heredoc to be rejected")
	}
}

func TestTransformLegacyHeredocsKeepsNativeCopy(t *testing.T) {
	input := []byte("FROM alpine\nCOPY <<'EOF' /file\nhello\nEOF\n")
	got, changed, err := TransformLegacyHeredocs(input)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("native Dockerfile heredoc should not be rewritten")
	}
	if string(got) != string(input) {
		t.Fatalf("unexpected change:\n%s", got)
	}
}

func TestRejectUnsupportedRunHeredocs(t *testing.T) {
	for name, header := range map[string]string{
		"chain":                "RUN mkdir -p /opt/demo && cat > /opt/demo/app.conf << 'EOF'",
		"continuation":         "RUN mkdir -p /opt/demo && \\\n    chmod 755 /opt/demo && \\\n    cat > /opt/demo/app.conf << 'EOF'",
		"comment continuation": "RUN true && \\\n# comment\n\n cat > /f <<EOF",
		"no spaces":            "RUN true&&cat>/f<<EOF",
		"reverse redirect":     "RUN true; cat <<EOF >/f",
		"append reverse":       "RUN cat <<EOF >>/f",
		"pipe":                 "RUN cat <<EOF | tee /f",
		"subshell":             "RUN (cat >/f <<EOF)",
		"conditional":          "RUN if true; then cat >/f <<EOF",
		"tee":                  "RUN tee /f <<EOF",
		"flags":                "RUN --mount=type=cache,target=/tmp cat >/f <<EOF",
		"variable target":      "RUN cat > $FILE <<EOF",
		"expanded target":      "RUN cat > ~/f <<EOF",
		"command suffix":       "RUN cat > /f <<EOF && chmod 755 /f",
		"lowercase":            "run true && cat >/f <<EOF",
	} {
		t.Run(name, func(t *testing.T) {
			input := []byte("FROM alpine\n\n" + header + "\nlisten_port=5140\nEOF\n")
			got, changed, err := TransformLegacyHeredocs(input)
			if err == nil || !strings.Contains(err.Error(), "line 3:") || !strings.Contains(err.Error(), "COPY") {
				t.Fatalf("expected actionable error on RUN line, got %v", err)
			}
			if got != nil || changed {
				t.Fatal("failure must not return a partial rewrite")
			}
		})
	}
}

func TestHeredocPassThroughBoundaries(t *testing.T) {
	for name, input := range map[string]string{
		"quoted operator":      "FROM alpine\nRUN echo '<<EOF' \"<<OTHER\"\n",
		"escaped operator":     "FROM alpine\nRUN echo \\<\\<EOF\n",
		"comment":              "FROM alpine\nRUN echo hi # <<EOF\n",
		"here string":          "FROM alpine\nRUN cat <<<hello\n",
		"shift":                "FROM alpine\nRUN echo $((1 << SHIFT))\n",
		"json":                 "FROM alpine\nRUN [\"echo\", \"<<EOF\"]\n",
		"json shell syntax":    "FROM alpine\nRUN [\"echo\", \"` <<EOF\"]\nCOPY [\"` <<EOF\", \"/file\"]\n",
		"json with flags":      "FROM alpine\nRUN --mount=type=cache,target=/tmp [\"echo\", \"` <<EOF\"]\nCOPY --chown=1000 [\"` <<EOF\", \"/file\"]\n",
		"native run":           "FROM alpine\nRUN <<'EOF'\nRUN cat > /f <<INNER\nEOF\n",
		"native run flags":     "FROM alpine\nRUN --mount=type=cache,target=/tmp <<'EOF'\necho hello\nEOF\n",
		"onbuild copy":         "FROM alpine\nONBUILD COPY <<EOF /f\nRUN cat >> /f <<INNER\nEOF\n",
		"copy filename":        "FROM alpine\nCOPY foo<<bar /file\nCOPY [\"<<EOF\", \"/file\"]\n",
		"copy body":            "FROM alpine\nCOPY <<'EOF' /script\nRUN true && cat >/f <<INNER\nEOF\n",
		"multiple copy bodies": "FROM alpine\nCOPY <<A <<B /out/\nRUN cat >/f <<INNER\nA\nRUN cat >> /f <<INNER\nB\n",
		"tab stripping":        "FROM alpine\nCOPY <<-EOF /f\n\tRUN cat > /g <<INNER\n\tEOF\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, changed, err := TransformLegacyHeredocs([]byte(input))
			if err != nil || changed || string(got) != input {
				t.Fatalf("expected byte-exact pass-through; changed=%v error=%v\n%s", changed, err, got)
			}
		})
	}
}

func TestRewriteContinuedStandaloneAndSkipItsBody(t *testing.T) {
	input := "FROM alpine\n  run cat > /file \\\n <<'EOF'\nRUN cat >> /other <<INNER\nEOF\nRUN cat >/ignored <<NOPE\n"
	// Missing space after cat is outside the legacy rewrite contract, but must
	// still be rejected rather than forwarded with a misleading frontend error.
	if _, _, err := TransformLegacyHeredocs([]byte(input)); err == nil || !strings.Contains(err.Error(), "line 6:") {
		t.Fatalf("unexpected error: %v", err)
	}
	input = strings.Split(input, "RUN cat >/ignored")[0]
	want := "FROM alpine\n  COPY <<'EOF' /file\nRUN cat >> /other <<INNER\nEOF\n"
	got, changed, err := TransformLegacyHeredocs([]byte(input))
	if err != nil || !changed || string(got) != want {
		t.Fatalf("changed=%v err=%v\n%s", changed, err, got)
	}
}

func TestHeredocEscapeDirectiveAndCRLF(t *testing.T) {
	input := "# escape=`\r\nFROM alpine\r\nRUN true && `\r\n cat >/f <<EOF\r\nhello\r\nEOF\r\n"
	if _, _, err := TransformLegacyHeredocs([]byte(input)); err == nil || !strings.Contains(err.Error(), "line 3:") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPreprocessFailurePreservesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Dockerfile")
	input := []byte("FROM alpine\nRUN cat > /first <<EOF\nhello\nEOF\nRUN true && cat >/second <<EOF\nworld\nEOF\n")
	if err := os.WriteFile(path, input, 0o640); err != nil {
		t.Fatal(err)
	}
	changed, err := PreprocessDockerfile(path)
	if err == nil || changed {
		t.Fatalf("expected rejection: changed=%v err=%v", changed, err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != string(input) {
		t.Fatalf("rejection changed the source file: %v", readErr)
	}
}

func FuzzTransformLegacyHeredocs(f *testing.F) {
	for _, command := range []string{"cat >/f <<'EOF'", "true && cat >/f <<EOF", "echo '<<'", "echo $((1<<N))", "cat <<", "if true; then cat <<EOF"} {
		f.Add(command)
	}
	f.Fuzz(func(t *testing.T, command string) {
		TransformLegacyHeredocs([]byte("FROM alpine\nRUN " + command + "\n"))
	})
}
