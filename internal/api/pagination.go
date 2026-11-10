package api

import (
	"context"
	"errors"
	"net/url"
)

// Page is the shape of every list the API answers (EventList, RequestLogList,
// StoreList ...): newest first, keyset-paginated. Cursor is opaque, null on the
// last page; HasMore says whether to read another.
type Page[T any] struct {
	Data    []T     `json:"data"`
	HasMore bool    `json:"has_more"`
	Cursor  *string `json:"cursor"`
}

// errStuckCursor guards the loop against a server answering the cursor it was
// just given.
var errStuckCursor = errors.New("api: the list answered the same cursor twice")

// Paginate reads a list endpoint page by page, calling each for every item until
// each returns false (stop) or an error, or the list ends. q is copied; its
// "cursor" is managed here and must not be set by the caller.
func Paginate[T any](ctx context.Context, c *Client, ep Endpoint, q url.Values, each func(T) (bool, error)) error {
	query := url.Values{}
	for k, v := range q {
		query[k] = append([]string(nil), v...)
	}
	last := ""
	for {
		var page Page[T]
		if _, err := c.DoJSON(ctx, ep, Call{Query: query}, &page); err != nil {
			return err
		}
		for _, item := range page.Data {
			more, err := each(item)
			if err != nil {
				return err
			}
			if !more {
				return nil
			}
		}
		if !page.HasMore || page.Cursor == nil || *page.Cursor == "" {
			return nil
		}
		if *page.Cursor == last {
			return errStuckCursor
		}
		last = *page.Cursor
		query.Set("cursor", last)
	}
}
