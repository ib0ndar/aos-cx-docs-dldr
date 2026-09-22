package pdfgen

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"
)

type processObservation struct {
	PeakRSS int64
	Err     error
}

func monitorProcessGroup(ctx context.Context, cancel context.CancelFunc, processGroup int, maxRSS int64) <-chan processObservation {
	return monitorNamedProcessGroup(ctx, cancel, processGroup, maxRSS, "Chrome")
}

func monitorNamedProcessGroup(
	ctx context.Context,
	cancel context.CancelFunc,
	processGroup int,
	maxRSS int64,
	name string,
) <-chan processObservation {
	result := make(chan processObservation, 1)
	go func() {
		defer close(result)
		observation := processObservation{}
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			rss, err := processGroupRSS(ctx, processGroup)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					result <- observation
					return
				}
				observation.Err = fmt.Errorf("measure %s process-group RSS: %w", name, err)
				cancel()
				result <- observation
				return
			}
			if rss > observation.PeakRSS {
				observation.PeakRSS = rss
			}
			if rss > maxRSS {
				observation.Err = fmt.Errorf("%s process-group RSS %d exceeds %d-byte limit", name, rss, maxRSS)
				cancel()
				result <- observation
				return
			}
			select {
			case <-ctx.Done():
				result <- observation
				return
			case <-ticker.C:
			}
		}
	}()
	return result
}

type limitedBuffer struct {
	buffer *bytes.Buffer
	limit  int
}

func (w *limitedBuffer) Write(data []byte) (int, error) {
	if len(data) > w.limit-w.buffer.Len() {
		return 0, errors.New("process accounting output exceeds limit")
	}
	return w.buffer.Write(data)
}
