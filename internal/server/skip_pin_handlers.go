// skip_pin_handlers.go 提供拼团小刀自动免拼名单的 REST 端点。
//
// 名单决定订单同步发现带 SKIP_PIN（直接免拼）按钮的订单时，哪些商品会被自动免拼。
// 端点挂在 /api/v1/accounts/{cid}/skip-pin 下，鉴权与归属校验与其他账号端点一致。
package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// mountSkipPinSettings 封装mount小刀免拼名单端点业务协调。
func (s *Server) mountSkipPinSettings(r chi.Router) {
	r.Get("/api/v1/accounts/{cid}/skip-pin", s.listSkipPinSettings)
	r.Put("/api/v1/accounts/{cid}/skip-pin", s.upsertSkipPinSetting)
	r.Delete("/api/v1/accounts/{cid}/skip-pin/{item_id}", s.deleteSkipPinSetting)
}

// skipPinEntry 是名单条目的 HTTP 响应形态；不暴露任何凭证字段。
type skipPinEntry struct {
	// ItemID 是闲鱼商品 ID。
	ItemID string `json:"item_id"`
	// Enabled 表示该商品当前是否启用自动免拼。
	Enabled bool `json:"enabled"`
	// CreatedAt 是首次登记时间（Unix 秒）。
	CreatedAt int64 `json:"created_at"`
	// UpdatedAt 是最近一次变更时间（Unix 秒）。
	UpdatedAt int64 `json:"updated_at"`
}

// skipPinListResponse 是名单读取响应。
type skipPinListResponse struct {
	// Items 是全部名单条目。
	Items []skipPinEntry `json:"items"`
}

// skipPinMutationResponse 是新增/更新/删除操作的统一响应。
type skipPinMutationResponse struct {
	// OK 固定为 true，表示操作已生效。
	OK bool `json:"ok"`
}

// listSkipPinSettings 返回账号的自动免拼名单。
func (s *Server) listSkipPinSettings(w http.ResponseWriter, r *http.Request) {
	// cid 用于本次流程后续判断的cid
	cid := chi.URLParam(r, "cid")
	if !s.ownsAccount(r, cid) {
		writeErr(w, http.StatusForbidden, "无权访问该账号")
		return
	}
	// port 是名单用例；测试环境未装配时返回服务不可用而不是 panic。
	port := s.skipPinSettingsApplication()
	if port == nil {
		writeErr(w, http.StatusServiceUnavailable, "小刀免拼名单未初始化")
		return
	}
	// items、err 是名单条目与读取错误。
	// items、err 是名单条目与读取错误。
	items, err := port.List(r.Context(), cid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取小刀免拼名单失败")
		return
	}
	// entries 是响应 DTO 列表。
	entries := make([]skipPinEntry, 0, len(items))
	// item 表示当前遍历过程中的名单条目。
	for _, item := range items {
		entries = append(entries, skipPinEntry{ItemID: item.ItemID, Enabled: item.Enabled, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, skipPinListResponse{Items: entries})
}

// skipPinUpsertRequest 是新增/更新名单条目的请求体。
type skipPinUpsertRequest struct {
	// ItemID 是闲鱼商品 ID（数字字符串）。
	ItemID string `json:"item_id"`
	// Enabled 为指针以区分「未传」与「显式关闭」。
	Enabled *bool `json:"enabled"`
}

// upsertSkipPinSetting 新增或更新一条名单；item_id 必须是数字字符串。
func (s *Server) upsertSkipPinSetting(w http.ResponseWriter, r *http.Request) {
	// cid 用于本次流程后续判断的cid
	cid := chi.URLParam(r, "cid")
	if !s.ownsAccount(r, cid) {
		writeErr(w, http.StatusForbidden, "无权操作该账号")
		return
	}
	// req 是请求体；enabled 为指针以区分「未传」与「传 false」。
	var req skipPinUpsertRequest
	if decodeJSON(r, &req) != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	// itemID 是规范化后的商品 ID。
	itemID := strings.TrimSpace(req.ItemID)
	if itemID == "" {
		writeErr(w, http.StatusBadRequest, "商品 ID 不能为空")
		return
	}
	// err 是商品 ID 数字格式校验错误。
	if _, err := strconv.ParseInt(itemID, 10, 64); err != nil {
		writeErr(w, http.StatusBadRequest, "商品 ID 必须是数字")
		return
	}
	// port 是名单用例。
	port := s.skipPinSettingsApplication()
	if port == nil {
		writeErr(w, http.StatusServiceUnavailable, "小刀免拼名单未初始化")
		return
	}
	// enabled 默认为启用，显式传 false 时关闭。
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	// err 是名单写入错误。
	if err := port.Upsert(r.Context(), cid, itemID, enabled); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存小刀免拼名单失败")
		return
	}
	writeJSON(w, http.StatusOK, skipPinMutationResponse{OK: true})
}

// deleteSkipPinSetting 移除一条名单；幂等。
func (s *Server) deleteSkipPinSetting(w http.ResponseWriter, r *http.Request) {
	// cid 用于本次流程后续判断的cid
	cid := chi.URLParam(r, "cid")
	if !s.ownsAccount(r, cid) {
		writeErr(w, http.StatusForbidden, "无权操作该账号")
		return
	}
	// itemID 是路径中的商品 ID。
	itemID := strings.TrimSpace(chi.URLParam(r, "item_id"))
	if itemID == "" {
		writeErr(w, http.StatusBadRequest, "商品 ID 不能为空")
		return
	}
	// port 是名单用例。
	port := s.skipPinSettingsApplication()
	if port == nil {
		writeErr(w, http.StatusServiceUnavailable, "小刀免拼名单未初始化")
		return
	}
	// err 是名单删除错误。
	if err := port.Delete(r.Context(), cid, itemID); err != nil {
		writeErr(w, http.StatusInternalServerError, "删除小刀免拼名单失败")
		return
	}
	writeJSON(w, http.StatusOK, skipPinMutationResponse{OK: true})
}
