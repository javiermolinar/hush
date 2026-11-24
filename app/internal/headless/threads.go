package headless

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/javiermolinar/hush/sdk"
)

func threadCommand(ctx context.Context, c *sdk.Client, o options, out io.Writer) error {
	switch o.command {
	case listThreads:
		page, err := c.ListThreadDirectory(ctx, o.directory)
		if err != nil {
			return err
		}
		if o.json {
			return writeValue(out, true, page, "")
		}
		for _, row := range page.Threads {
			if _, err := fmt.Fprintln(out, threadLine(row)); err != nil {
				return err
			}
		}
		if page.Indexing {
			_, err := fmt.Fprintln(out, "Partial thread list; run again when indexing finishes.")
			return err
		}
		if page.NextCursor != nil {
			_, err := fmt.Fprintln(out, "Next cursor:", *page.NextCursor)
			return err
		}
		return nil
	case newThread:
		if o.create.CWD == "" && o.socket == "" && os.Getenv("HUSH_SOCKET") == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("find working directory: %w", err)
			}
			o.create.CWD = cwd
		}
		row, err := c.CreateThread(ctx, o.create)
		if err != nil {
			return fmt.Errorf("create thread: %w", err)
		}
		return writeValue(out, o.json, row, threadLine(row))
	case forkThread:
		thread, err := c.Fork(ctx, o.thread, o.atEntry)
		if err != nil {
			return fmt.Errorf("fork thread: %w", err)
		}
		return writeValue(out, o.json, thread, fmt.Sprintf("thread %s forked from %s at %s", thread.ID, o.thread, o.atEntry))
	case archiveThread, unarchiveThread:
		archived := o.command == archiveThread
		if err := c.SetThreadArchived(ctx, o.thread, archived); err != nil {
			return err
		}
		value := struct {
			ID       sdk.ID `json:"id"`
			Archived bool   `json:"archived"`
		}{o.thread, archived}
		verb := "unarchived"
		if archived {
			verb = "archived"
		}
		return writeValue(out, o.json, value, fmt.Sprintf("thread %s %s", o.thread, verb))
	}
	return sdk.ErrProtocol
}

func threadLine(row sdk.ThreadDirectoryRow) string {
	line := string(row.ID)
	if row.Status != "" {
		line += "  " + row.Status
	}
	if row.Archived {
		line += "  archived"
	}
	if row.Title != "" {
		line += "  " + strconv.QuoteToGraphic(row.Title)
	}
	if row.ParentID != "" {
		line += "  fork of #" + string(row.ParentID)
	}
	if row.Checkout != nil && row.Checkout.Kind == "managed" {
		line += "  " + strconv.QuoteToGraphic(row.Checkout.Branch) + "  " + strconv.QuoteToGraphic(row.Checkout.CWD)
	}
	return line
}
