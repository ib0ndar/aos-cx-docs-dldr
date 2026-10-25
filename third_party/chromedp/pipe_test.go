package chromedp

// Local aos-cx-docs-dldr patch tests for the DevTools pipe transport. Run
// them from the application module root with
// `go test github.com/chromedp/chromedp`, which resolves this replacement
// with the application's own dependency versions.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto"
)

func TestPipeConnFramesMessagesWithNUL(t *testing.T) {
	commandRead, commandWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	responseRead, responseWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer commandRead.Close()
	defer responseWrite.Close()
	conn := newPipeConn(responseRead, commandWrite)
	defer conn.Close()

	if err := conn.Write(context.Background(), &cdproto.Message{ID: 7, Method: "Browser.getVersion"}); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 256)
	n, err := commandRead.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if written := buffer[:n]; !bytes.HasSuffix(written, []byte{0}) || bytes.Count(written, []byte{0}) != 1 ||
		!bytes.Contains(written, []byte(`"Browser.getVersion"`)) {
		t.Fatalf("command was not one NUL-terminated JSON message: %q", written)
	}

	if _, err := responseWrite.Write([]byte("{\"id\":7,\"result\":{}}\x00{\"method\":\"Target.targetCreated\",\"params\":{}}\x00{\"id\":")); err != nil {
		t.Fatal(err)
	}
	var first, second cdproto.Message
	if err := conn.Read(context.Background(), &first); err != nil || first.ID != 7 {
		t.Fatalf("first response = %+v, %v", first, err)
	}
	if err := conn.Read(context.Background(), &second); err != nil || second.Method != "Target.targetCreated" {
		t.Fatalf("second message = %+v, %v", second, err)
	}
	responseWrite.Close()
	var truncated cdproto.Message
	if err := conn.Read(context.Background(), &truncated); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated final message was not rejected: %v", err)
	}
}

func TestPipeConnCloseUnblocksRead(t *testing.T) {
	responseRead, responseWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer responseWrite.Close()
	commandRead, commandWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer commandRead.Close()
	conn := newPipeConn(responseRead, commandWrite)
	result := make(chan error, 1)
	go func() {
		var message cdproto.Message
		result <- conn.Read(context.Background(), &message)
	}()
	time.Sleep(50 * time.Millisecond)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("read succeeded after close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock a pending Read")
	}
	if _, err := commandRead.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("closing the connection did not close Chrome's command pipe: %v", err)
	}
}

func TestStartWithPipeRefusesForeignExtraFiles(t *testing.T) {
	cmd := exec.Command("/nonexistent/chrome")
	cmd.ExtraFiles = []*os.File{os.Stdin}
	if _, err := startWithPipe(cmd); err == nil || !strings.Contains(err.Error(), "ExtraFiles") {
		t.Fatalf("foreign descriptors 3 and 4 were accepted: %v", err)
	}
	if cmd.Process != nil {
		t.Fatal("process started despite the descriptor conflict")
	}
}
