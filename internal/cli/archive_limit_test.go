package cli

import "testing"

func TestPDFResourceLimitDoesNotDependOnHTMLAggregateLimit(t *testing.T) {
	options := options{
		platform: "6300", version: "10.10", guides: []string{"jobscheduler"}, destination: t.TempDir(),
		portal: "https://example.test/portal", transport: "http", timeout: 45, attemptTimeout: 45, retries: 0,
		maxMB: 2048, maxArchiveMB: 1024,
	}
	if _, err := options.validate(); err != nil {
		t.Fatalf("valid PDF-oriented resource limit was rejected: %v", err)
	}
}
