package pdfgen

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestChromeThatDidNotStartIsLaunchedOnceMore(t *testing.T) {
	output, err := os.Create(filepath.Join(t.TempDir(), "out.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	startFailure := &chromeStartError{errors.New("start pinned Chrome: chrome did not answer on the DevTools pipe")}
	for _, test := range []struct {
		name    string
		results []error
		calls   int
		wantErr bool
	}{
		{"started", []error{nil}, 1, false},
		{"answered on the second launch", []error{startFailure, nil}, 2, false},
		{"never answered", []error{startFailure, startFailure}, 2, true},
		{"failed while rendering", []error{errors.New("print to PDF failed")}, 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			_, err := retryChromeStart(context.Background(), output, func() (renderMetrics, error) {
				calls++
				if calls == 1 {
					if _, err := output.WriteString("partial"); err != nil {
						t.Fatal(err)
					}
				} else if info, err := output.Stat(); err != nil || info.Size() != 0 {
					t.Fatalf("output was not emptied before the second launch: %v %v", info, err)
				}
				return renderMetrics{}, test.results[calls-1]
			})
			if calls != test.calls || (err != nil) != test.wantErr {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	if _, err := retryChromeStart(ctx, output, func() (renderMetrics, error) {
		calls++
		return renderMetrics{}, startFailure
	}); err == nil || calls != 1 {
		t.Fatalf("a cancelled render was launched again: calls=%d err=%v", calls, err)
	}
}
