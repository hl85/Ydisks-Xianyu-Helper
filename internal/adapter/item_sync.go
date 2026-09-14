package adapter

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	itemapp "xianyu-go/internal/application/items"
	"xianyu-go/internal/db"
	"xianyu-go/internal/xianyu/cookierefresh"
	"xianyu-go/internal/xianyu/mtop"
)

// ItemSyncRepository 将商品同步应用 Port 适配到数据库、MTOP 和账号运行时。
type ItemSyncRepository struct {
	// store 提供账号凭证和商品持久化能力。
	store *db.Store
	// client 返回商品列表和详情平台调用能力，允许运行时注入测试客户端。
	client func() mtop.Client
	// logger 记录不含凭证的同步阶段信息。
	logger *slog.Logger
	// specProbe 记录「账号 + 商品」最近一次成功向平台确认多规格的时间，用于避免每次同步重复请求详情接口。
	specProbe map[string]time.Time
	// specProbeMu 保护 specProbe 的并发读写。
	specProbeMu sync.Mutex
	// specProbeTTL 是远端多规格结果的复用时长；到期后重新向平台确认以支持双向变化，为零表示每次都重新探测。
	specProbeTTL time.Duration
	// updateRunningCookie 将平台返回的新 Cookie 同步到运行中的账号实例。
	updateRunningCookie func(context.Context, string, string)
	// recoverExpiredSession 在平台报告 Session 或 MTOP Token 过期时触发账号恢复。
	recoverExpiredSession func(context.Context, string, error)
}

// NewItemSyncRepository 构造商品同步基础设施适配器。
func NewItemSyncRepository(store *db.Store, client func() mtop.Client, logger *slog.Logger, updateRunningCookie func(context.Context, string, string), recoverExpiredSession func(context.Context, string, error)) *ItemSyncRepository {
	if client == nil {
		client = func() mtop.Client { return mtop.NewClient() }
	}
	if logger == nil {
		logger = slog.Default()
	}
	// repository 保存完成默认值填充后的商品同步适配器。
	repository := &ItemSyncRepository{store: store, client: client, logger: logger, updateRunningCookie: updateRunningCookie, recoverExpiredSession: recoverExpiredSession, specProbe: make(map[string]time.Time), specProbeTTL: multiSpecProbeTTLDefault}
	return repository
}

// OwnsAccount 按非敏感所有者字段判断用户是否拥有账号。
func (r *ItemSyncRepository) OwnsAccount(ctx context.Context, userID int64, cookieID string) (bool, error) {
	if r == nil || r.store == nil || r.store.Cookies == nil {
		return false, syncStageError(itemapp.SyncErrorPersistence, errors.New("商品同步存储未初始化"))
	}
	// ownerID、ownerErr 保存只读取账号归属的查询结果。
	ownerID, ownerErr := r.store.Cookies.GetOwnerID(ctx, cookieID)
	if errors.Is(ownerErr, db.ErrNotFound) {
		return false, nil
	}
	if ownerErr != nil {
		return false, syncStageError(itemapp.SyncErrorPersistence, ownerErr)
	}
	return ownerID == userID, nil
}

// SyncAll 读取平台商品全集、探测多规格并完成本地 reconcile。
func (r *ItemSyncRepository) SyncAll(ctx context.Context, query itemapp.SyncQuery) (itemapp.SyncAllResult, error) {
	// requestCtx、cancel 控制全量同步的最长执行时间。
	requestCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// latest、cookieValue、requestContext、session、unlock 保存远端请求使用的凭证快照及锁。
	latest, cookieValue, requestContext, session, unlock, err := r.begin(requestCtx, query)
	if err != nil {
		return itemapp.SyncAllResult{}, err
	}
	// unlock 在创建完整 Cookie 会话后立即释放，避免慢速平台 I/O 占用账号锁。
	unlock()
	// result、callErr 保存平台全集结果和调用错误。
	result, callErr := r.mtopClient().FetchAllItems(requestContext, cookieValue, query.PageSize, query.MaxPages)
	// latest、session 和 callErr 通过重新加锁的提交阶段处理平台 Cookie 变化。
	latest, session, callErr, err = r.finishRemote(requestCtx, query, latest, session, callErr)
	if err != nil {
		return itemapp.SyncAllResult{}, err
	}
	if callErr != nil {
		r.recoverExpired(requestCtx, query.CookieID, callErr)
		return itemapp.SyncAllResult{}, syncStageError(itemapp.SyncErrorPlatform, callErr)
	}
	if result == nil {
		return itemapp.SyncAllResult{}, syncStageError(itemapp.SyncErrorPlatform, errors.New("商品列表接口未返回结果"))
	}
	// detailCookies 保存规格探测使用的最新 Cookie 串。
	detailCookies := cookieValue
	if result.UpdatedCookies != "" {
		detailCookies = result.UpdatedCookies
	}
	// enrichOutcome 保存本轮多规格探测的次数与首个错误；探测失败只降级，不再中断同步。
	enrichOutcome := r.enrichMultiSpec(requestContext, detailCookies, query.CookieID, result.Items)
	r.reportMultiSpecDegraded(requestContext, query.CookieID, enrichOutcome)
	// persistErr 保存探测后 Cookie 会话提交错误。
	persistErr := r.persistAfterEnrich(requestCtx, query, latest, session, cookieValue, result.UpdatedCookies)
	if persistErr != nil {
		return itemapp.SyncAllResult{}, persistErr
	}
	// syncResult、syncErr 保存本地全集 reconcile 结果及错误。
	syncResult, syncErr := r.syncItems(requestCtx, query.CookieID, result.Items)
	if syncErr != nil {
		return itemapp.SyncAllResult{}, syncStageError(itemapp.SyncErrorPersistence, syncErr)
	}
	return itemapp.SyncAllResult{TotalCount: len(result.Items), TotalPages: result.TotalPages, SavedCount: syncResult.Saved, DeletedCount: syncResult.Deleted}, nil
}

// SyncPage 读取平台指定页、探测多规格并保存本页商品。
func (r *ItemSyncRepository) SyncPage(ctx context.Context, query itemapp.SyncQuery) (itemapp.SyncPageResult, error) {
	// requestCtx、cancel 控制分页同步的最长执行时间。
	requestCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	// latest、cookieValue、requestContext、session、unlock 保存远端请求使用的凭证快照及锁。
	latest, cookieValue, requestContext, session, unlock, err := r.begin(requestCtx, query)
	if err != nil {
		return itemapp.SyncPageResult{}, err
	}
	unlock()
	// result、callErr 保存平台分页结果和调用错误。
	result, callErr := r.mtopClient().FetchItemsPage(requestContext, cookieValue, query.PageNumber, query.PageSize)
	// latest、session 和 callErr 通过提交阶段处理平台 Cookie 变化。
	latest, session, callErr, err = r.finishRemote(requestCtx, query, latest, session, callErr)
	if err != nil {
		return itemapp.SyncPageResult{}, err
	}
	if callErr != nil {
		r.recoverExpired(requestCtx, query.CookieID, callErr)
		return itemapp.SyncPageResult{}, syncStageError(itemapp.SyncErrorPlatform, callErr)
	}
	if result == nil {
		return itemapp.SyncPageResult{}, syncStageError(itemapp.SyncErrorPlatform, errors.New("商品列表接口未返回结果"))
	}
	// detailCookies 保存规格探测使用的最新 Cookie 串。
	detailCookies := cookieValue
	if result.UpdatedCookies != "" {
		detailCookies = result.UpdatedCookies
	}
	// enrichOutcome 保存本页多规格探测的次数与首个错误；探测失败只降级，不再中断同步。
	enrichOutcome := r.enrichMultiSpec(requestContext, detailCookies, query.CookieID, result.Items)
	r.reportMultiSpecDegraded(requestContext, query.CookieID, enrichOutcome)
	// persistErr 保存探测后 Cookie 会话提交错误。
	persistErr := r.persistAfterEnrich(requestCtx, query, latest, session, cookieValue, result.UpdatedCookies)
	if persistErr != nil {
		return itemapp.SyncPageResult{}, persistErr
	}
	// saved 保存本页成功写入商品的数量。
	saved, saveErr := r.saveItems(requestCtx, query.CookieID, result.Items)
	if saveErr != nil {
		return itemapp.SyncPageResult{}, syncStageError(itemapp.SyncErrorPersistence, saveErr)
	}
	return itemapp.SyncPageResult{PageNumber: result.PageNumber, PageSize: result.PageSize, CurrentCount: len(result.Items), SavedCount: saved}, nil
}

// begin 读取并校验账号平台视图，同时创建本次请求的 Cookie 会话。
func (r *ItemSyncRepository) begin(ctx context.Context, query itemapp.SyncQuery) (*db.CookiePlatformRuntimeData, string, context.Context, *mtop.CookieSession, func(), error) {
	if r == nil || r.store == nil || r.store.Cookies == nil || r.store.Items == nil {
		return nil, "", ctx, nil, func() {}, syncStageError(itemapp.SyncErrorPersistence, errors.New("商品同步存储未初始化"))
	}
	// unlock 保护账号凭证快照的读取和提交。
	unlock := r.store.LockAccountCredentials(query.CookieID)
	// latest、loadErr 保存平台凭证视图及读取错误。
	latest, loadErr := r.store.Cookies.GetCookiePlatformRuntimeData(ctx, query.CookieID)
	if errors.Is(loadErr, db.ErrNotFound) {
		unlock()
		return nil, "", ctx, nil, func() {}, itemapp.ErrSyncNotOwned
	}
	if loadErr != nil {
		unlock()
		return nil, "", ctx, nil, func() {}, syncStageError(itemapp.SyncErrorPersistence, loadErr)
	}
	if latest.UserID != query.UserID {
		unlock()
		return nil, "", ctx, nil, func() {}, itemapp.ErrSyncNotOwned
	}
	if !hasStoredCredential(latest) {
		unlock()
		return nil, "", ctx, nil, func() {}, syncStageError(itemapp.SyncErrorCredential, errors.New("账号凭证已变化，请重试"))
	}
	// requestContext、session 保存平台调用需要的 Cookie 会话上下文。
	requestContext, session := withCookieSnapshot(ctx, latest)
	return &latest, latest.Value, requestContext, session, unlock, nil
}

// finishRemote 在远端调用后重新读取账号并提交 Cookie 会话变化。
func (r *ItemSyncRepository) finishRemote(ctx context.Context, query itemapp.SyncQuery, detail *db.CookiePlatformRuntimeData, session *mtop.CookieSession, callErr error) (*db.CookiePlatformRuntimeData, *mtop.CookieSession, error, error) {
	// unlock 保护远端调用完成后的凭证复核和写回。
	unlock := r.store.LockAccountCredentials(query.CookieID)
	// notifyValue 保存提交成功后需要同步给运行时的最新 Cookie；通知必须在释放账号锁后执行。
	notifyValue := ""
	defer func() {
		unlock()
		if notifyValue != "" {
			r.notifyRunningCookie(ctx, query.CookieID, notifyValue)
		}
	}()
	// latest、loadErr 保存远端调用后的最新账号凭证视图。
	latest, loadErr := r.store.Cookies.GetCookiePlatformRuntimeData(ctx, query.CookieID)
	if loadErr != nil {
		return nil, session, callErr, syncStageError(itemapp.SyncErrorPersistence, loadErr)
	}
	if latest.UserID != query.UserID {
		return nil, session, callErr, syncStageError(itemapp.SyncErrorCredential, errors.New("账号凭证已变化，请重试"))
	}
	// changed 表示远端调用期间凭证是否被其他流程更新。
	changed := latest.Value != detail.Value || latest.MetadataJSON != detail.MetadataJSON
	if changed {
		return nil, session, callErr, syncStageError(itemapp.SyncErrorCredential, errors.New("账号凭证已变化，请重试"))
	}
	// value、valueChanged、handled、persistErr 保存 Cookie 会话提交结果。
	value, valueChanged, handled, persistErr := r.persistSession(ctx, latest, session)
	if persistErr != nil {
		return nil, session, callErr, syncStageError(itemapp.SyncErrorPersistence, persistErr)
	}
	if handled && valueChanged {
		// 同步第一阶段已经写入新 Cookie，记录待释放账号锁后发送的运行时通知。
		notifyValue = value
	}
	if handled {
		// refreshed、refreshErr 读取提交后的凭证视图，避免详情探测阶段把已写入的 Cookie
		// 误判为并发更新而跳过后续会话保存。
		refreshed, refreshErr := r.store.Cookies.GetCookiePlatformRuntimeData(ctx, query.CookieID)
		if refreshErr != nil {
			return nil, session, callErr, syncStageError(itemapp.SyncErrorPersistence, refreshErr)
		}
		latest = refreshed
	}
	return &latest, session, callErr, nil
}

// persistAfterEnrich 在多规格探测后复核账号并写回平台 Cookie 变化。
func (r *ItemSyncRepository) persistAfterEnrich(ctx context.Context, query itemapp.SyncQuery, detail *db.CookiePlatformRuntimeData, session *mtop.CookieSession, originalCookie, updatedCookie string) error {
	// unlock 保护探测完成后的凭证复核和写回。
	unlock := r.store.LockAccountCredentials(query.CookieID)
	// notifyValue 保存提交成功后需要同步给运行时的最新 Cookie；通知必须在释放账号锁后执行。
	notifyValue := ""
	defer func() {
		unlock()
		if notifyValue != "" {
			r.notifyRunningCookie(ctx, query.CookieID, notifyValue)
		}
	}()
	// latest、loadErr 保存探测完成后的最新账号凭证视图。
	latest, loadErr := r.store.Cookies.GetCookiePlatformRuntimeData(ctx, query.CookieID)
	if loadErr != nil {
		return syncStageError(itemapp.SyncErrorPersistence, loadErr)
	}
	if latest.UserID != query.UserID {
		return syncStageError(itemapp.SyncErrorCredential, errors.New("账号凭证已变化，请重试"))
	}
	// changed 表示平台请求期间是否已有其他流程写入凭证。
	changed := latest.Value != detail.Value || latest.MetadataJSON != detail.MetadataJSON
	if !changed {
		// value、valueChanged、handled、persistErr 保存 Cookie 会话提交结果。
		value, valueChanged, handled, persistErr := r.persistSession(ctx, latest, session)
		if persistErr != nil {
			return syncStageError(itemapp.SyncErrorPersistence, persistErr)
		}
		if handled && valueChanged {
			notifyValue = value
		} else if !handled && updatedCookie != "" && updatedCookie != originalCookie {
			// saveErr 保存平台返回的新 Cookie 写回错误。
			if saveErr := r.store.Cookies.UpdateValueOwned(ctx, query.CookieID, updatedCookie, query.UserID); saveErr != nil {
				return syncStageError(itemapp.SyncErrorPersistence, saveErr)
			}
			notifyValue = updatedCookie
		}
	}
	return nil
}

// persistSession 保存完整 Cookie Jar 或平面 Cookie 的会话变化。
func (r *ItemSyncRepository) persistSession(ctx context.Context, detail db.CookiePlatformRuntimeData, session *mtop.CookieSession) (string, bool, bool, error) {
	if session == nil {
		return "", false, false, nil
	}
	// value、snapshot、changed 保存平台会话当前状态。
	value, snapshot, changed := session.State()
	if !changed {
		return detail.Value, false, snapshot != nil, nil
	}
	// metadata 保存移除旧快照后待写入的新 metadata。
	metadata := cookierefresh.MetadataWithoutSnapshot(detail.MetadataJSON)
	if snapshot != nil {
		metadata = cookierefresh.MetadataWithSnapshot(detail.MetadataJSON, snapshot)
	}
	// err 保存 Cookie 会话持久化错误。
	err := r.store.Cookies.UpdateRenewalCookie(ctx, detail.ID, value, metadata, time.Now().Unix())
	return value, value != detail.Value, true, err
}

// saveItems 在数据库事务内保存分页同步返回的商品基础信息并返回成功数。
func (r *ItemSyncRepository) saveItems(ctx context.Context, cookieID string, items []mtop.ItemListItem) (int, error) {
	// rows 保存从平台商品模型转换出的分页持久化记录。
	rows := make([]db.ItemInfoRow, 0, len(items))
	// item 表示当前待转换的平台商品。
	for _, item := range items {
		// price 保存优先使用格式化价格的商品价格文本。
		price := item.PriceText
		if price == "" {
			price = item.Price
		}
		rows = append(rows, db.ItemInfoRow{CookieID: cookieID, ItemID: item.ID, ItemTitle: item.Title, ItemCategory: item.CategoryID, ItemPrice: price, ItemDetail: item.ItemDetail, IsMultiSpec: item.IsMultiSpec})
	}
	return r.store.Items.SavePageFromRemote(ctx, cookieID, rows)
}

// syncItems 将平台商品模型转换为数据库行并执行全集 reconcile。
func (r *ItemSyncRepository) syncItems(ctx context.Context, cookieID string, items []mtop.ItemListItem) (db.ItemSyncResult, error) {
	// rows 保存待写入数据库的商品行。
	rows := make([]db.ItemInfoRow, 0, len(items))
	// item 表示当前待转换的平台商品。
	for _, item := range items {
		// price 保存优先使用格式化价格的商品价格文本。
		price := item.PriceText
		if price == "" {
			price = item.Price
		}
		rows = append(rows, db.ItemInfoRow{CookieID: cookieID, ItemID: item.ID, ItemTitle: item.Title, ItemCategory: item.CategoryID, ItemPrice: price, ItemDetail: item.ItemDetail, IsMultiSpec: item.IsMultiSpec})
	}
	return r.store.Items.SyncFromRemote(ctx, cookieID, rows)
}

// enrichMultiSpec 的详情探测失败不影响商品落库：多规格只是列表的补充事实，
// 平台风控或单品异常都必须退化为沿用本地或列表已有多规格标记，而不是中断整个同步。
func (r *ItemSyncRepository) enrichMultiSpec(ctx context.Context, cookies, cookieID string, items []mtop.ItemListItem) itemSyncEnrichOutcome {
	// fetcher、ok 保存可选商品详情探测能力及其存在状态。
	fetcher, ok := r.mtopClient().(mtop.ItemDetailFetcher)
	if !ok {
		return itemSyncEnrichOutcome{Reused: len(items)}
	}
	// candidates 保存本轮需要向平台确认的商品下标；近期已确认过远端结果的商品不再重复请求详情接口。
	candidates := r.multiSpecCandidates(cookieID, items)
	// outcome 保存本轮探测的次数与首个错误，供调用方记录与账号恢复使用。
	outcome := itemSyncEnrichOutcome{Reused: len(items) - len(candidates)}
	if len(candidates) == 0 {
		return outcome
	}
	// localMultiSpec 保存本地已持久化的多规格标记，探测失败时避免用未确认的结论覆盖已有事实。
	localMultiSpec := r.localMultiSpecSnapshot(ctx, cookieID)
	// probeCtx、cancel 控制批量详情探测的收束。
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// semaphore 限制同时进行的详情探测数量。
	semaphore := make(chan struct{}, multiSpecProbeConcurrency)
	// waitGroup 等待所有详情探测 goroutine 结束。
	var waitGroup sync.WaitGroup
	// stateMu 保护 outcome 与 stopping 的并发读写。
	var stateMu sync.Mutex
	// stopping 表示已出现平台风控，本轮剩余商品不得继续请求详情接口。
	stopping := false
	// index 表示当前需要启动详情探测的商品下标。
	for _, index := range candidates {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			select {
			case semaphore <- struct{}{}:
			case <-probeCtx.Done():
				return
			}
			defer func() { <-semaphore }()
			stateMu.Lock()
			if stopping {
				stateMu.Unlock()
				return
			}
			outcome.Probed++
			stateMu.Unlock()
			// isMultiSpec、detectErr 保存当前商品详情探测结果及错误。
			isMultiSpec, detectErr := fetcher.DetectItemMultiSpec(probeCtx, cookies, items[index].ID)
			if detectErr != nil {
				// fallback、known 保存本地已持久化的多规格标记及其存在状态。
				fallback, known := localMultiSpec[items[index].ID]
				if known {
					items[index].IsMultiSpec = fallback
				}
				stateMu.Lock()
				outcome.Fallback++
				if outcome.FirstErr == nil {
					outcome.FirstErr = detectErr
				}
				// platformRisk 表示本次失败属于必须立刻止损的平台风控而非普通业务错误。
				platformRisk := mtop.IsRiskVerificationErr(detectErr)
				if platformRisk {
					stopping = true
				}
				stateMu.Unlock()
				if platformRisk {
					// 风控期间继续请求详情只会延长封禁，因此收束本轮剩余探测。
					cancel()
				}
				return
			}
			items[index].IsMultiSpec = isMultiSpec
			r.markMultiSpecProbed(cookieID, items[index].ID)
		}(index)
	}
	waitGroup.Wait()
	return outcome
}

// reportMultiSpecDegraded 处理多规格探测未全部完成的情况：凭证类错误转交账号恢复，其余只记录告警。
// 商品多规格只是列表事实的补充，因此这里绝不返回错误，调用方必须继续完成商品落库。
func (r *ItemSyncRepository) reportMultiSpecDegraded(ctx context.Context, cookieID string, outcome itemSyncEnrichOutcome) {
	if outcome.FirstErr == nil {
		return
	}
	if mtop.IsCredentialRefreshableErr(outcome.FirstErr) {
		r.recoverExpired(ctx, cookieID, outcome.FirstErr)
	}
	// logger 保存可用的日志器，实例缺失时退化为默认日志器以保证降级过程可观测。
	logger := r.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("商品多规格探测未全部完成，已沿用本地或列表标记",
		"account", cookieID, "probed", outcome.Probed, "fallback", outcome.Fallback, "reason", outcome.FirstErr)
}

// multiSpecCandidates 挑选本轮需要向平台确认多规格的商品下标；近期已确认过的商品直接复用旧结果。
// 商品详情接口短时高频调用会触发平台风控，因此这里同时受单轮上限约束，剩余商品留给后续同步逐步确认。
func (r *ItemSyncRepository) multiSpecCandidates(cookieID string, items []mtop.ItemListItem) []int {
	// candidates 保存本轮待探测的商品下标，容量足够容纳全部商品以避免扩容。
	candidates := make([]int, 0, len(items))
	// now 保存本次筛选的统一基准时间，避免每个商品重复读取时钟。
	now := time.Now()
	// index 表示当前待判断是否复用旧结果的商品下标。
	for index := range items {
		if r.multiSpecFresh(cookieID, items[index].ID, now) {
			continue
		}
		candidates = append(candidates, index)
		if len(candidates) >= multiSpecProbeMaxPerRound {
			break
		}
	}
	return candidates
}

// multiSpecFresh 判断商品的多规格远端结果是否仍处于可复用有效期内。
func (r *ItemSyncRepository) multiSpecFresh(cookieID, itemID string, now time.Time) bool {
	if r.specProbeTTL <= 0 {
		return false
	}
	r.specProbeMu.Lock()
	// probedAt、ok 保存该商品上次成功确认的时间及其是否存在记录。
	probedAt, ok := r.specProbe[multiSpecProbeKey(cookieID, itemID)]
	r.specProbeMu.Unlock()
	return ok && now.Sub(probedAt) < r.specProbeTTL
}

// markMultiSpecProbed 记录商品刚刚成功确认过远端多规格事实，使其在复用期内不再重复请求详情接口。
func (r *ItemSyncRepository) markMultiSpecProbed(cookieID, itemID string) {
	if r.specProbe == nil {
		return
	}
	r.specProbeMu.Lock()
	r.specProbe[multiSpecProbeKey(cookieID, itemID)] = time.Now()
	r.specProbeMu.Unlock()
}

// multiSpecProbeKey 生成「账号 + 商品」的多规格复用键，避免不同账号的同名商品互相串用结果。
func multiSpecProbeKey(cookieID, itemID string) string {
	return cookieID + "\x00" + itemID
}

// localMultiSpecSnapshot 读取账号下已持久化的商品多规格标记，作为详情探测失败时的兜底事实。
func (r *ItemSyncRepository) localMultiSpecSnapshot(ctx context.Context, cookieID string) map[string]bool {
	// snapshot 保存商品标识到多规格标记的映射，查询失败时返回空集合而不是猜测商品事实。
	snapshot := make(map[string]bool)
	if r == nil || r.store == nil || r.store.Items == nil {
		return snapshot
	}
	// rows、err 保存本地商品记录及查询错误。
	rows, err := r.store.Items.AllForCookie(ctx, cookieID)
	if err != nil {
		// logger 保存可用的日志器，实例缺失时退化为默认日志器以保证读取失败可观测。
		logger := r.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("读取本地多规格兜底失败，探测失败时将沿用列表初判", "account", cookieID)
		return snapshot
	}
	// row 表示当前待收录的本地商品记录。
	for _, row := range rows {
		snapshot[row.ItemID] = row.IsMultiSpec
	}
	return snapshot
}

// multiSpecProbeConcurrency 是同时进行商品详情探测的最大数量；该接口短时高频调用会触发平台风控，必须保持极低并发。
const multiSpecProbeConcurrency = 2

// multiSpecProbeTTLDefault 是远端多规格结果的默认复用时长，到期后重新向平台确认，兼顾降频与双向变化。
const multiSpecProbeTTLDefault = 6 * time.Hour

// multiSpecProbeMaxPerRound 是单次同步最多发起的商品详情探测数量，超出部分留给后续同步逐步确认。
const multiSpecProbeMaxPerRound = 20

// itemSyncEnrichOutcome 描述一轮多规格探测的结果；详情探测只补充列表事实，
// 失败必须退化为沿用已有标记，因此它携带统计与首个错误，但不再作为同步失败原因。
type itemSyncEnrichOutcome struct {
	// FirstErr 保存首个商品详情探测错误，供日志与账号凭证恢复使用。
	FirstErr error
	// Probed 保存本轮真正向平台发起的详情探测次数，用于观察接口调用量。
	Probed int
	// Reused 保存因近期已向平台确认过而跳过探测的商品数量。
	Reused int
	// Fallback 保存因探测失败而改用本地已有多规格标记的商品数量。
	Fallback int
}

// withCookieSnapshot 创建带完整 Cookie Jar 或平面 Cookie 的平台上下文。
func withCookieSnapshot(ctx context.Context, detail db.CookiePlatformRuntimeData) (context.Context, *mtop.CookieSession) {
	// snapshot、ok 保存 metadata 中的完整 Cookie Jar 快照及解析状态。
	snapshot, ok := cookierefresh.SnapshotFromMetadataOK(detail.MetadataJSON)
	if !ok {
		return mtop.WithFlatCookieSession(ctx, detail.Value)
	}
	return mtop.WithCookieSnapshot(ctx, snapshot)
}

// hasStoredCredential 判断账号平台视图是否包含可用 Cookie 凭证。
func hasStoredCredential(detail db.CookiePlatformRuntimeData) bool {
	if strings.TrimSpace(detail.Value) != "" {
		return true
	}
	// complete 表示 metadata 是否包含完整 Cookie Jar。
	_, complete := cookierefresh.SnapshotFromMetadataOK(detail.MetadataJSON)
	return complete
}

// syncStageError 将底层错误包装为不泄露基础设施类型的应用错误。
func syncStageError(kind itemapp.SyncErrorKind, err error) error {
	return &itemapp.SyncError{Kind: kind, Err: err}
}

// notifyRunningCookie 将平台返回的 Cookie 更新到运行中的账号实例。
func (r *ItemSyncRepository) notifyRunningCookie(ctx context.Context, cookieID, value string) {
	if r.updateRunningCookie != nil && value != "" {
		r.updateRunningCookie(ctx, cookieID, value)
	}
}

// recoverExpired 在平台会话过期时通知账号恢复协调器。
func (r *ItemSyncRepository) recoverExpired(ctx context.Context, cookieID string, err error) {
	if r.recoverExpiredSession != nil && err != nil {
		r.recoverExpiredSession(ctx, cookieID, err)
	}
}

// mtopClient 返回当前商品同步使用的平台客户端，并在测试构造缺失时提供默认实现。
func (r *ItemSyncRepository) mtopClient() mtop.Client {
	if r != nil && r.client != nil {
		// client 表示当前回调返回的平台客户端。
		if client := r.client(); client != nil {
			return client
		}
	}
	return mtop.NewClient()
}
