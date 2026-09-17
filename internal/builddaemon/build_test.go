package builddaemon

import (
	"errors"
	"testing"
)

func TestOutputAttrs(t *testing.T) {
	nydus := outputAttrs("example.com/ns/repo:tag_nydus_v3", imageTypeNydus)
	if nydus["compression"] != "nydus" || nydus["fs-version"] != "5" || nydus["push"] != "true" {
		t.Fatalf("unexpected nydus attrs: %#v", nydus)
	}
	oci := outputAttrs("example.com/ns/repo:tag", imageTypeOCI)
	if oci["compression"] != "gzip" || oci["name"] != "example.com/ns/repo:tag" {
		t.Fatalf("unexpected oci attrs: %#v", oci)
	}
}

func TestBuildStepsForBothFormats(t *testing.T) {
	steps := buildStepsForImageType("example.com/ns/repo:tag", imageTypeBoth)
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
	if steps[0].Format != imageTypeOCI || steps[0].Image != "example.com/ns/repo:tag" {
		t.Fatalf("unexpected first step: %#v", steps[0])
	}
	if steps[1].Format != imageTypeNydus || steps[1].Image != "example.com/ns/repo:tag_nydus_v3" {
		t.Fatalf("unexpected second step: %#v", steps[1])
	}
}

func TestDeterministicBuildError(t *testing.T) {
	for _, err := range []error{
		errors.New(`process "/bin/sh -c false" did not complete successfully: exit code: 1`),
		errors.New("dockerfile parse error on line 3: unknown instruction"),
		errors.New("failed to parse dockerfile: syntax error"),
	} {
		if !isDeterministicBuildError(err) {
			t.Fatalf("expected deterministic error: %v", err)
		}
	}
	if isDeterministicBuildError(errors.New("unexpected status from PATCH request: 502 Bad Gateway")) {
		t.Fatal("registry 502 must remain retryable")
	}
}

func TestRetryableBuildkitAddrErrorRequiresBuildkitAddressForDialErrors(t *testing.T) {
	workerErr := errors.New("rpc error: code = Unavailable desc = connection error: desc = \"transport: Error while dialing: dial tcp 10.0.0.1:9094: connect: connection refused\"")
	if !isRetryableBuildkitAddrError(workerErr, "tcp://10.0.0.1:9094") {
		t.Fatal("expected worker dial failure to be retryable")
	}

	registryErr := errors.New(`failed to solve: image export stage: export default image: failed to do request: Head "http://localhost:5000/v2/node/blobs/sha256:abc": dial tcp [::1]:5000: connect: connection refused`)
	if isRetryableBuildkitAddrError(registryErr, "tcp://10.0.0.1:9094") {
		t.Fatal("expected target registry connection failure not to trigger worker failover")
	}
}
