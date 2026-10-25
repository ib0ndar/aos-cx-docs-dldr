package chromedp

// Local aos-cx-docs-dldr patch: DevTools transport over Chrome's
// --remote-debugging-pipe file descriptors. See UPSTREAM.md.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/chromedp/cdproto"
	cdpbrowser "github.com/chromedp/cdproto/browser"
	jsonv2 "github.com/go-json-experiment/json"
)

// pipeConn implements Transport over the two anonymous pipes Chrome opens
// with --remote-debugging-pipe: Chrome reads commands from file descriptor 3
// and writes responses and events to file descriptor 4. Every message is one
// JSON object terminated by a NUL byte. JSON encoding escapes U+0000, so a raw
// NUL never occurs inside a message.
//
// Browser reads from one goroutine and writes from another; the two
// directions use separate files, so no locking is needed. Close unblocks a
// pending Read because pipe files use the runtime poller.
type pipeConn struct {
	reader *bufio.Reader
	in     *os.File // parent end of Chrome's fd 4 (responses, events)
	out    *os.File // parent end of Chrome's fd 3 (commands)

	debugf func(string, ...any)
}

func newPipeConn(in, out *os.File) *pipeConn {
	return &pipeConn{reader: bufio.NewReader(in), in: in, out: out}
}

// Read reads the next NUL-terminated message.
func (c *pipeConn) Read(_ context.Context, msg *cdproto.Message) error {
	data, err := c.reader.ReadBytes(0)
	if err != nil {
		if errors.Is(err, io.EOF) && len(data) != 0 {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	data = data[:len(data)-1]
	if c.debugf != nil {
		c.debugf("<- %s", data)
	}
	return jsonv2.Unmarshal(data, msg, DefaultUnmarshalOptions)
}

// Write writes one NUL-terminated message.
func (c *pipeConn) Write(_ context.Context, msg *cdproto.Message) error {
	data, err := jsonv2.Marshal(msg, DefaultMarshalOptions)
	if err != nil {
		return err
	}
	if c.debugf != nil {
		c.debugf("-> %s", data)
	}
	_, err = c.out.Write(append(data, 0))
	return err
}

// Close closes both parent pipe ends. Closing the command pipe makes Chrome
// see end-of-file on fd 3, after which it shuts itself down.
func (c *pipeConn) Close() error {
	return errors.Join(c.out.Close(), c.in.Close())
}

// startWithPipe starts cmd with the DevTools pipes as its file descriptors 3
// and 4 and returns this process's ends as a connection. The child's ends are
// closed here once inherited, so that a Chrome exit is seen as end-of-file.
func startWithPipe(cmd *exec.Cmd) (*pipeConn, error) {
	if len(cmd.ExtraFiles) != 0 {
		return nil, errors.New("RemoteDebuggingPipe needs file descriptors 3 and 4, but ExtraFiles is set")
	}
	commandRead, commandWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	responseRead, responseWrite, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, commandRead.Close(), commandWrite.Close())
	}
	cmd.ExtraFiles = []*os.File{commandRead, responseWrite}
	err = cmd.Start()
	commandRead.Close()
	responseWrite.Close()
	if err != nil {
		return nil, errors.Join(err, commandWrite.Close(), responseRead.Close())
	}
	return newPipeConn(responseRead, commandWrite), nil
}

// allocatePipeBrowser finishes ExecAllocator.Allocate in pipe mode. It
// mirrors the websocket path: a lost connection cancels the browser. Chrome
// prints no URL here, so the same startup timeout instead bounds its first
// answer on the pipe.
func (a *ExecAllocator) allocatePipeBrowser(
	ctx context.Context, c *Context, cmd *exec.Cmd, dataDir string, conn *pipeConn, opts []BrowserOption,
) (*Browser, error) {
	opts = append(append([]BrowserOption{}, opts...), withTransport(conn))
	browser, err := NewBrowser(ctx, "", opts...)
	if err != nil {
		conn.Close()
		return nil, err
	}
	go func() {
		<-browser.LostConnection
		select {
		case <-browser.closingGracefully:
		default:
			c.cancel()
		}
	}()
	browser.process = cmd.Process
	browser.userDataDir = dataDir

	readyCtx, cancel := context.WithTimeout(ctx, a.wsURLReadTimeout)
	defer cancel()
	if err := browser.execute(readyCtx, cdpbrowser.CommandGetVersion, nil, nil); err != nil {
		return nil, fmt.Errorf("chrome did not answer on the DevTools pipe: %w", err)
	}
	return browser, nil
}
