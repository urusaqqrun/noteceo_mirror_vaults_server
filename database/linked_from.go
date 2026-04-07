package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
)

func nowMillis() int64 { return time.Now().UnixMilli() }

var cubelvLinkRe = regexp.MustCompile(`\[([^\]]*)\]\((?:cubelv|CubeLV)://(\w+)/([a-zA-Z0-9_-]+)[^)]*\)`)

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

func buildBacklinkString(sourceName, sourceType, sourceID string) string {
	return fmt.Sprintf("[%s](cubelv://%s/%s)", sourceName, strings.ToLower(sourceType), sourceID)
}

func linkedFromContainsID(linkedFrom []string, sourceID string) bool {
	suffix := "/" + sourceID + ")"
	for _, entry := range linkedFrom {
		if strings.HasSuffix(entry, suffix) || strings.Contains(entry, "/"+sourceID+"?") {
			return true
		}
	}
	return false
}

func removeFromLinkedFrom(linkedFrom []string, sourceID string) []string {
	suffix := "/" + sourceID + ")"
	alt := "/" + sourceID + "?"
	result := make([]string, 0, len(linkedFrom))
	for _, entry := range linkedFrom {
		if !strings.HasSuffix(entry, suffix) && !strings.Contains(entry, alt) {
			result = append(result, entry)
		}
	}
	return result
}

// ProcessBacklinksOnContentChange 在 UpsertItem 中呼叫：
// 比較 oldContent 和 newContent，更新目標 items 的 linked_from
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

	backlinkStr := buildBacklinkString(sourceName, sourceType, sourceID)

	for _, targetID := range added {
		s.addBacklink(ctx, tx, targetID, sourceID, backlinkStr, actorID)
	}
	for _, targetID := range removed {
		s.removeBacklink(ctx, tx, targetID, sourceID, actorID)
	}
}

func (s *PgStore) addBacklink(ctx context.Context, tx *sql.Tx, targetID, sourceID, backlinkStr, actorID string) {
	var raw string
	var ver int
	err := tx.QueryRowContext(ctx,
		`SELECT linked_from, version FROM base_items WHERE id = $1`, targetID,
	).Scan(&raw, &ver)
	if err != nil {
		return
	}
	var arr []string
	json.Unmarshal([]byte(raw), &arr)
	if linkedFromContainsID(arr, sourceID) {
		return
	}
	arr = append(arr, backlinkStr)
	data, _ := json.Marshal(arr)
	newVer := ver + 1
	if _, err := tx.ExecContext(ctx,
		`UPDATE base_items SET linked_from = $1, version = $2 WHERE id = $3`,
		string(data), newVer, targetID,
	); err != nil {
		log.Printf("[LinkedFrom] mirror add backlink to %s failed: %v", targetID, err)
		return
	}
	tx.ExecContext(ctx,
		`INSERT INTO sync_changes (item_id, item_version, change_type, actor_id, details, created_at)
		 VALUES ($1, $2, 'updated', $3, 'backlink-add', $4)`,
		targetID, newVer, actorID, nowMillis(),
	)
}

func (s *PgStore) removeBacklink(ctx context.Context, tx *sql.Tx, targetID, sourceID, actorID string) {
	var raw string
	var ver int
	err := tx.QueryRowContext(ctx,
		`SELECT linked_from, version FROM base_items WHERE id = $1`, targetID,
	).Scan(&raw, &ver)
	if err != nil {
		return
	}
	var arr []string
	json.Unmarshal([]byte(raw), &arr)
	cleaned := removeFromLinkedFrom(arr, sourceID)
	if len(cleaned) == len(arr) {
		return
	}
	data, _ := json.Marshal(cleaned)
	newVer := ver + 1
	if _, err := tx.ExecContext(ctx,
		`UPDATE base_items SET linked_from = $1, version = $2 WHERE id = $3`,
		string(data), newVer, targetID,
	); err != nil {
		log.Printf("[LinkedFrom] mirror remove backlink from %s failed: %v", targetID, err)
		return
	}
	tx.ExecContext(ctx,
		`INSERT INTO sync_changes (item_id, item_version, change_type, actor_id, details, created_at)
		 VALUES ($1, $2, 'updated', $3, 'backlink-remove', $4)`,
		targetID, newVer, actorID, nowMillis(),
	)
}
