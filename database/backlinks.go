package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"time"
)

func nowMillis() int64 { return time.Now().UnixMilli() }

var cubelvLinkRe = regexp.MustCompile(`\[([^\]]*)\]\((?:cubelv|CubeLV)://(\w+)/([a-zA-Z0-9_-]+)[^)]*\)`)

type BacklinkEntry struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

func extractLinkTargetIDs(content string) map[string]bool {
	if content == "" {
		return nil
	}
	matches := cubelvLinkRe.FindAllStringSubmatch(content, -1)
	ids := make(map[string]bool, len(matches))
	for _, m := range matches {
		ids[m[3]] = true
	}
	return ids
}

func backlinksContain(entries []BacklinkEntry, sourceID, entryType string) bool {
	for _, e := range entries {
		if e.ID == sourceID && e.Type == entryType {
			return true
		}
	}
	return false
}

func removeFromBacklinks(entries []BacklinkEntry, sourceID, entryType string) []BacklinkEntry {
	result := make([]BacklinkEntry, 0, len(entries))
	for _, e := range entries {
		if !(e.ID == sourceID && e.Type == entryType) {
			result = append(result, e)
		}
	}
	return result
}

func (s *PgStore) ProcessBacklinksOnContentChange(
	ctx context.Context, tx *sql.Tx,
	sourceID, sourceName, sourceType string,
	oldContent, newContent string,
	actorID string,
) {
	oldIDs := extractLinkTargetIDs(oldContent)
	newIDs := extractLinkTargetIDs(newContent)

	var added, removed []string
	for id := range newIDs {
		if !oldIDs[id] && id != sourceID {
			added = append(added, id)
		}
	}
	for id := range oldIDs {
		if !newIDs[id] && id != sourceID {
			removed = append(removed, id)
		}
	}

	if len(added) == 0 && len(removed) == 0 {
		return
	}

	for _, targetID := range added {
		s.addBacklink(ctx, tx, targetID, sourceID, "mention", actorID)
	}
	for _, targetID := range removed {
		s.removeBacklink(ctx, tx, targetID, sourceID, "mention", actorID)
	}
}

func (s *PgStore) ProcessBacklinksOnSubjectChange(
	ctx context.Context, tx *sql.Tx,
	sourceID string,
	oldSubjectID, newSubjectID string,
	actorID string,
) {
	if oldSubjectID == newSubjectID {
		return
	}
	if oldSubjectID != "" {
		s.removeBacklink(ctx, tx, oldSubjectID, sourceID, "child", actorID)
	}
	if newSubjectID != "" {
		s.addBacklink(ctx, tx, newSubjectID, sourceID, "child", actorID)
	}
}

func (s *PgStore) addBacklink(ctx context.Context, tx *sql.Tx, targetID, sourceID, entryType, actorID string) {
	var raw string
	var ver int
	err := tx.QueryRowContext(ctx,
		`SELECT backlinks, version FROM base_items WHERE id = $1`, targetID,
	).Scan(&raw, &ver)
	if err != nil {
		return
	}
	var arr []BacklinkEntry
	json.Unmarshal([]byte(raw), &arr)
	if backlinksContain(arr, sourceID, entryType) {
		return
	}
	arr = append(arr, BacklinkEntry{ID: sourceID, Type: entryType})
	data, _ := json.Marshal(arr)
	newVer := ver + 1
	if _, err := tx.ExecContext(ctx,
		`UPDATE base_items SET backlinks = $1, version = $2 WHERE id = $3`,
		string(data), newVer, targetID,
	); err != nil {
		log.Printf("[Backlinks] mirror add backlink to %s failed: %v", targetID, err)
		return
	}
	tx.ExecContext(ctx,
		`INSERT INTO sync_changes (item_id, item_version, change_type, actor_id, details, created_at)
		 VALUES ($1, $2, 'updated', $3, 'backlink-add', $4)`,
		targetID, newVer, actorID, nowMillis(),
	)
}

func (s *PgStore) removeBacklink(ctx context.Context, tx *sql.Tx, targetID, sourceID, entryType, actorID string) {
	var raw string
	var ver int
	err := tx.QueryRowContext(ctx,
		`SELECT backlinks, version FROM base_items WHERE id = $1`, targetID,
	).Scan(&raw, &ver)
	if err != nil {
		return
	}
	var arr []BacklinkEntry
	json.Unmarshal([]byte(raw), &arr)
	cleaned := removeFromBacklinks(arr, sourceID, entryType)
	if len(cleaned) == len(arr) {
		return
	}
	data, _ := json.Marshal(cleaned)
	newVer := ver + 1
	if _, err := tx.ExecContext(ctx,
		`UPDATE base_items SET backlinks = $1, version = $2 WHERE id = $3`,
		string(data), newVer, targetID,
	); err != nil {
		log.Printf("[Backlinks] mirror remove backlink from %s failed: %v", targetID, err)
		return
	}
	tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO sync_changes (item_id, item_version, change_type, actor_id, details, created_at)
		 VALUES ($1, $2, 'updated', $3, 'backlink-remove', $4)`),
		targetID, newVer, actorID, nowMillis(),
	)
}
