package mirror

// ConflictResolution 衝突解決結果
type ConflictResolution string

const (
	ResolutionApplyAI   ConflictResolution = "apply_ai"   // AI 的修改較新，套用
	ResolutionKeepUser  ConflictResolution = "keep_user"   // 用戶的修改較新，保留
	ResolutionSkip      ConflictResolution = "skip"        // 跳過（無法判定）
)

// ConflictContext 衝突判定所需的上下文
type ConflictContext struct {
	AIStartUSN int   // AI 開始執行時的 USN
	AIEndUSN   int   // AI frontmatter 中的 USN
	DBUSN      int   // 目前 MongoDB 中的 USN
	AITimestamp int64 // AI 修改的時間戳
	DBTimestamp int64 // DB 最後更新的時間戳
}

// ResolveConflict 判定 AI 修改與用戶修改的優先順序
//
// 規則：
//  1. AI 的 USN >= DB USN → apply_ai（AI 基於最新版本修改）
//  2. DB USN > AI 開始時的 USN → keep_user（用戶在 AI 執行期間有新修改）
//  3. 同 USN 比時間戳 → 較新的獲勝
func ResolveConflict(ctx ConflictContext) ConflictResolution {
	// AI USN 嚴格大於 DB USN → AI 比 DB 新
	if ctx.AIEndUSN > ctx.DBUSN {
		return ResolutionApplyAI
	}

	// AI USN == DB USN → 看 AI 是否有成長（有實際修改）
	if ctx.AIEndUSN == ctx.DBUSN {
		// AI 有從 start 成長到 DB 水準 → 套用 AI
		if ctx.AIEndUSN > ctx.AIStartUSN {
			return ResolutionApplyAI
		}
		// AI 沒改（USN 沒增長），比較時間戳
		if ctx.AITimestamp > ctx.DBTimestamp {
			return ResolutionApplyAI
		}
		if ctx.DBTimestamp > ctx.AITimestamp {
			return ResolutionKeepUser
		}
		return ResolutionSkip
	}

	// DB USN > AI USN → 用戶較新
	return ResolutionKeepUser
}

// ShouldApplyImport 判定單筆回寫是否應套用
func ShouldApplyImport(entry ImportEntry, dbUSN int, aiStartUSN int) ConflictResolution {
	switch entry.Action {
	case ImportActionCreate:
		// 新建永遠套用
		return ResolutionApplyAI

	case ImportActionDelete:
		// 刪除：如果 DB USN 在 AI 期間沒增加，套用
		if dbUSN <= aiStartUSN {
			return ResolutionApplyAI
		}
		return ResolutionKeepUser

	case ImportActionUpdate, ImportActionMove:
		entryUSN := 0
		if entry.NoteMeta != nil {
			entryUSN = entry.NoteMeta.USN
		}
		if entry.FolderMeta != nil {
			entryUSN = entry.FolderMeta.USN
		}
		if entry.CardMeta != nil {
			entryUSN = entry.CardMeta.USN
		}

		return ResolveConflict(ConflictContext{
			AIStartUSN: aiStartUSN,
			AIEndUSN:   entryUSN,
			DBUSN:      dbUSN,
		})

	default:
		return ResolutionSkip
	}
}
