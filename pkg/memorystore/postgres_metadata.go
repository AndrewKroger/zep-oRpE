package memorystore

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/uptrace/bun"

	"github.com/getzep/zep/pkg/models"
)

// putMessageMetadata stores a new or updates existing message metadata. Existing
// metadata is determined by message UUID. isPrivileged is used to determine if
// the caller is allowed to store metadata in the `system` top-level key.
// Unprivileged callers will have the `system` key removed from the metadata.
// Can be enrolled in an existing transaction by passing a bun.Tx as db.
func putMessageMetadata(
	ctx context.Context,
	db bun.IDB,
	sessionID string,
	messageMetaSet []models.MessageMetadata,
	isPrivileged bool,
) error {
	var tx bun.Tx
	var err error

	// remove the top-level `system` key from the metadata if the caller is not privileged
	if !isPrivileged {
		messageMetaSet = removeSystemMetadata(messageMetaSet)
	}

	tx, isDBTransaction := db.(bun.Tx)
	if !isDBTransaction {
		// db is not already a transaction, so begin one
		if tx, err = db.BeginTx(ctx, &sql.TxOptions{}); err != nil {
			return NewStorageError("failed to begin transaction", err)
		}
		defer rollbackOnError(tx)
	}

	for i := range messageMetaSet {
		err := putMessageMetadataTx(ctx, tx, sessionID, &messageMetaSet[i])
		if err != nil {
			// defer will roll back the transaction
			return NewStorageError("failed to put message metadata", err)
		}
	}

	if !isDBTransaction {
		if err = tx.Commit(); err != nil {
			return NewStorageError("failed to commit transaction", err)
		}
	}

	return nil
}

// removeSystemMetadata removes the top-level `system` key from the metadata. This
// is used to prevent unprivileged callers from storing metadata in the `system` tree.
func removeSystemMetadata(metadata []models.MessageMetadata) []models.MessageMetadata {
	filteredMessageMetadata := make([]models.MessageMetadata, 0)

	for _, m := range metadata {
		if m.Key != "system" && !strings.HasPrefix(m.Key, "system.") {
			delete(m.Metadata, "system")
			filteredMessageMetadata = append(filteredMessageMetadata, m)
		}
	}
	return filteredMessageMetadata
}

func putMessageMetadataTx(
	ctx context.Context,
	tx bun.Tx,
	sessionID string,
	messageMetadata *models.MessageMetadata,
) error {
	// TODO: simplify all of this by getting `jsonb_set` working in bun

	err := acquireAdvisoryLock(ctx, tx, sessionID+messageMetadata.UUID.String())
	if err != nil {
		return NewStorageError("failed to acquire advisory lock", err)
	}

	var msg PgMessageStore
	err = tx.NewSelect().Model(&msg).
		Column("metadata").
		Where("session_id = ? AND uuid = ?", sessionID, messageMetadata.UUID).
		Scan(ctx)
	if err != nil {
		return NewStorageError(
			"failed to retrieve existing metadata. was the session deleted?",
			err,
		)
	}

	if msg.Metadata == nil {
		msg.Metadata = make(map[string]interface{})
	}

	err = storeMetadataByPath(
		msg.Metadata,
		strings.Split(messageMetadata.Key, "."),
		messageMetadata.Metadata,
	)
	if err != nil {
		return NewStorageError("failed to store metadata by path", err)
	}

	msg.UUID = messageMetadata.UUID
	_, err = tx.NewUpdate().
		Model(&msg).
		Column("metadata").
		Where("session_id = ? AND uuid = ?", sessionID, messageMetadata.UUID).
		Exec(ctx)
	if err != nil {
		return NewStorageError("failed to update message metadata", err)
	}

	return nil
}

// storeMetadataByPath takes a value map, a key path, and metadata as input arguments.
// It stores the metadata in the nested map structure referenced by the key path.
// If the key path is empty or contains only an empty string, the function merges
// the metadata into the value map. If a key in the path does not exist or is nil,
// it creates a new map at that key. The function returns an error if metadata must
// be a map but is not of type map[string]interface{}.
func storeMetadataByPath(
	value map[string]interface{},
	keyPath []string,
	metadata interface{},
) error {
	length := len(keyPath)
	if length == 0 || (length == 1 && keyPath[0] == "") {
		metadataMap, ok := metadata.(map[string]interface{})
		if !ok {
			return errors.New("metadata must be of type map[string]interface{}")
		}
		for k, v := range metadataMap {
			value[k] = v
		}
		return nil
	}

	for idx, key := range keyPath {
		isLastKey := idx == length-1

		if existingValue, ok := value[key]; ok && isLastKey {
			existingMap, existingOk := existingValue.(map[string]interface{})
			metadataMap, metadataOk := metadata.(map[string]interface{})
			if existingOk && metadataOk {
				for k, v := range metadataMap {
					existingMap[k] = v
				}
				return nil
			}
		}

		if isLastKey {
			value[key] = metadata
		} else {
			childValue, ok := value[key]
			if !ok || childValue == nil {
				childValue = make(map[string]interface{})
				value[key] = childValue
			}
			value = childValue.(map[string]interface{})
		}
	}

	return nil
}
