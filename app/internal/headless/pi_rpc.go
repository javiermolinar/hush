package headless

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/javiermolinar/hush/sdk"
)

// RPC stdin and the Unix stream are independent. Only this loop writes stdout;
// it consumes verified responses and events in socket order without blocking
// input behind a long command (e.g. abort can interrupt manual compaction).
func runRPC(ctx context.Context, stream *sdk.PiStream, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type inputEnd struct {
		sent int
		err  error
	}
	ended := make(chan inputEnd, 1)
	invalid := make(chan sdk.PiResponse)
	go func() {
		var end inputEnd
		defer func() { ended <- end }()
		r := sdk.NewPiLineReader(in)
		for {
			line, err := r.Read()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					end.err = err
				}
				return
			}
			cmd, err := sdk.ParsePiCommand(line)
			if err != nil {
				response := sdk.PiResponse{Type: "response", Command: cmd.Type, ID: cmd.ID, Error: "Invalid command fields"}
				if response.Command == "" {
					response.Command, response.ID, response.Error = "parse", nil, "Failed to parse command"
				}
				select {
				case invalid <- response:
				case <-ctx.Done():
					return
				}
				continue
			}
			if err := stream.Send(ctx, cmd); err != nil {
				end.err = err
				return
			}
			end.sent++
		}
	}()
	type observed struct {
		row sdk.PiRecord
		err error
	}
	live := make(chan observed)
	go func() {
		for {
			row, err := stream.Next(ctx)
			select {
			case live <- observed{row, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	replied, target := 0, -1
	var grace *time.Timer
	var elapsed <-chan time.Time
	defer func() {
		if grace != nil {
			grace.Stop()
		}
	}()
	for target < 0 || replied < target {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case end := <-ended:
			if end.err != nil {
				return end.err
			}
			// EOF is disposal, not agent settlement. Give outstanding replies a
			// bounded flush; do not join a compactor or pretend a run aborted.
			target = end.sent
			grace = time.NewTimer(time.Second)
			elapsed = grace.C
		case <-elapsed:
			return nil
		case response := <-invalid:
			if err := objectLine(out, response); err != nil {
				return err
			}
		case record := <-live:
			if record.err != nil {
				return record.err
			}
			if record.row.Response != nil {
				if err := objectLine(out, record.row.Response); err != nil {
					return err
				}
				replied++
			} else if err := writeLine(out, record.row.Event.Event); err != nil {
				return err
			}
		}
	}
	return nil
}
