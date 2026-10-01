package cmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/urfave/cli/v2"
)

// newTestApp returns the CLI with stdout/stderr captured and exit handling disabled.
func newTestApp() (*cli.App, *bytes.Buffer, *bytes.Buffer) {
	app := New("test", "none", "now")
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app.Writer = stdout
	app.ErrWriter = stderr
	app.ExitErrHandler = func(*cli.Context, error) {}
	return app, stdout, stderr
}

func TestExitErrHandlerNilError(t *testing.T) {
	// urfave/cli invokes the handler with a nil error after a successful command
	assert.NotPanics(t, func() { New("test", "none", "now").ExitErrHandler(nil, nil) })
}
