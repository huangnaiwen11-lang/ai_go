package r2

import (
	"context"
	"fmt"

	"ai-business-service/internal/biz/r2audit"
)

// NewAuditInventory adapts the public R2 client to the deliberately
// read-only, managed-result inventory used by the orphan audit command.
func NewAuditInventory(client *Client) r2audit.Inventory {
	return &auditInventory{client: client}
}

type auditInventory struct {
	client *Client
}

// List permits only the B2B result namespace. Callers cannot turn the orphan
// audit into a general public-bucket lister by supplying another prefix.
func (inventory *auditInventory) List(ctx context.Context, prefix, cursor string, limit int32) (r2audit.Page, error) {
	if inventory == nil || inventory.client == nil {
		return r2audit.Page{}, ErrClientUnavailable
	}
	if prefix != r2audit.ManagedResultPrefix {
		return r2audit.Page{}, r2audit.ErrInvalidCommand
	}
	listed, err := inventory.client.List(ctx, ListInput{
		Prefix: prefix, MaxKeys: limit, ContinuationToken: cursor,
	})
	if err != nil {
		return r2audit.Page{}, err
	}
	if listed.IsTruncated && listed.NextContinuationToken == "" {
		return r2audit.Page{}, fmt.Errorf("R2 audit list is truncated without a continuation cursor")
	}
	if !listed.IsTruncated && listed.NextContinuationToken != "" {
		return r2audit.Page{}, fmt.Errorf("R2 audit list has an unexpected continuation cursor")
	}
	page := r2audit.Page{Objects: make([]r2audit.Object, 0, len(listed.Contents))}
	for _, object := range listed.Contents {
		storageKey := inventory.client.config.PublicObjectURL(object.Key)
		if storageKey == "" {
			return r2audit.Page{}, ErrInvalidObjectRequest
		}
		page.Objects = append(page.Objects, r2audit.Object{Key: object.Key, StorageKey: storageKey})
	}
	if listed.IsTruncated {
		page.NextCursor = listed.NextContinuationToken
	}
	return page, nil
}

var _ r2audit.Inventory = (*auditInventory)(nil)
