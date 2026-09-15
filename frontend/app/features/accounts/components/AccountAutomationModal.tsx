import { CalendarClock,Loader2,MessageSquareQuote,Play,Save,Sparkles,X } from 'lucide-react';
import React from 'react';
import { AccountDetail,AccountTaskSettings,deleteSkipPinSetting,listSkipPinSettings,SkipPinEntry,upsertSkipPinSetting } from '../api';
import { useAccountAutomation } from '../accountAutomationHooks';

interface Props {
  /** account 表示账号。 */ account: AccountDetail;
  /** onClose 表示关闭弹窗的回调。 */ onClose: () => void;
  /** onSaved 表示保存完成后的回调。 */ onSaved: (settings: AccountTaskSettings) => void;
}

// Toggle 渲染可复用的开关控件。
const Toggle: React.FC<{/** checked 表示开关当前是否选中。 */ checked: boolean; /** onChange 表示开关状态变化的回调。 */ onChange: () => void; /** label 表示控件的无障碍名称。 */ label: string}> = ({ checked, onChange, label }) => (
  <button type="button" aria-label={label} aria-pressed={checked} onClick={onChange}
    className={`relative h-8 w-14 shrink-0 rounded-full transition-colors ${checked ? 'bg-sky-500' : 'bg-slate-300'}`}>
    <span className={`absolute left-1 top-1 h-6 w-6 rounded-full bg-white shadow-sm transition-transform ${checked ? 'translate-x-6' : ''}`} />
  </button>
);

// SkipPinSection 渲染小刀自动免拼名单管理区块。
const SkipPinSection: React.FC<{/** accountId 表示账号 ID。 */ accountId: string}> = ({ accountId }) => {
  // entries 是当前名单。
  const [entries, setEntries] = React.useState<SkipPinEntry[]>([]);
  // loading 表示名单读取中。
  const [loading, setLoading] = React.useState(true);
  // busy 表示任一写操作进行中。
  const [busy, setBusy] = React.useState(false);
  // newItemId 是输入框中的商品 ID。
  const [newItemId, setNewItemId] = React.useState('');
  // error 是操作失败提示。
  const [error, setError] = React.useState('');

  // load 拉取最新名单。
  const load = React.useCallback(/* 当前回调处理用户交互或异步状态变化。 */async () => {
    setLoading(true);
    try {
      // res 是名单接口返回。
      const res = await listSkipPinSettings(accountId);
      setEntries(res.items);
      setError('');
    } catch {
      setError('名单读取失败，请稍后重试');
    } finally {
      setLoading(false);
    }
  }, [accountId]);

  React.useEffect(() => { void load(); }, [load]);

  // mutate 执行写操作并刷新名单。
  const mutate = /* 当前回调处理用户交互或异步状态变化。 */async (action: () => Promise<unknown>) => {
    setBusy(true);
    try {
      await action();
      await load();
      setError('');
    } catch {
      setError('操作失败，请稍后重试');
    } finally {
      setBusy(false);
    }
  };

  // addItem 登记新商品（仅接受数字 ID）。
  const addItem = () => {
    // itemId 是规范化后的商品 ID。
    const itemId = newItemId.trim();
    if (!itemId || !/^\d+$/.test(itemId)) {
      setError('请输入数字商品 ID');
      return;
    }
    setNewItemId('');
    void mutate(/* 当前回调处理用户交互或异步状态变化。 */() => upsertSkipPinSetting(accountId, itemId, true));
  };

  return (
    <section className="rounded-2xl border border-slate-200 bg-white p-5">
      <div className="flex items-start gap-4">
        <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-sky-50 text-sky-600"><Sparkles className="h-5 w-5" /></div>
        <div className="min-w-0 flex-1">
          <h4 className="font-black text-slate-900">小刀自动免拼</h4>
          <p className="mt-1 text-xs leading-5 text-slate-500">买家拍下名单内商品的小刀单后，系统自动点「直接免拼」并随即自动发货，无需人工盯守。</p>
          <div className="mt-4 flex items-center gap-2">
            <input aria-label="商品 ID" inputMode="numeric" placeholder="输入商品 ID（纯数字）" value={newItemId}
              onChange={/* 当前回调处理用户交互或异步状态变化。 */ event => setNewItemId(event.target.value)}
              onKeyDown={/* 当前回调处理用户交互或异步状态变化。 */ event => { if (event.key === 'Enter') { addItem(); } }}
              className="min-w-0 flex-1 rounded-xl border border-slate-200 bg-slate-50 px-3 py-2 text-sm font-bold text-slate-800 placeholder:font-normal placeholder:text-slate-400 outline-none focus:border-sky-400 focus:ring-2 focus:ring-sky-100" />
            <button type="button" disabled={busy || !newItemId.trim()} onClick={/* 当前回调处理用户交互或异步状态变化。 */ () => addItem()}
              className="shrink-0 rounded-xl bg-sky-500 px-4 py-2 text-xs font-extrabold text-white hover:bg-sky-600 disabled:opacity-50">添加</button>
          </div>
          {loading && <div className="mt-3 text-xs text-slate-400">名单读取中...</div>}
          {error && <div className="mt-3 text-xs font-medium text-red-600">{error}</div>}
          {!loading && entries.length === 0 && <div className="mt-3 text-xs text-slate-400">名单为空：登记商品后，该商品的小刀单将自动免拼。</div>}
          {entries.length > 0 && (
            <ul className="mt-3 space-y-2">
              {entries.map(/* 当前回调处理用户交互或异步状态变化。 */entry => (
                <li key={entry.item_id} className="flex items-center justify-between gap-3 rounded-xl bg-slate-50 px-3 py-2">
                  <div className="min-w-0">
                    <div className="truncate text-sm font-extrabold text-slate-800">{entry.item_id}</div>
                    <div className="text-[11px] text-slate-400">登记于 {new Date(entry.created_at * 1000).toLocaleString('zh-CN')}</div>
                  </div>
                  <div className="flex shrink-0 items-center gap-2">
                    <Toggle checked={entry.enabled} label={`商品 ${entry.item_id} 自动免拼开关`} onChange={/* 当前回调处理用户交互或异步状态变化。 */() => void mutate(/* 当前回调处理用户交互或异步状态变化。 */() => upsertSkipPinSetting(accountId, entry.item_id, !entry.enabled))} />
                    <button type="button" disabled={busy} aria-label={`移除商品 ${entry.item_id}`}
                      onClick={/* 当前回调处理用户交互或异步状态变化。 */() => void mutate(/* 当前回调处理用户交互或异步状态变化。 */() => deleteSkipPinSetting(accountId, entry.item_id))}
                      className="rounded-lg p-1.5 text-slate-400 hover:bg-red-50 hover:text-red-600 disabled:opacity-50">
                      <X className="h-4 w-4" />
                    </button>
                  </div>
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>
    </section>
  );
};

// AccountAutomationModal 渲染账号自动化设置弹窗。
const AccountAutomationModal: React.FC<Props> = ({ account, onClose, onSaved }) => {
  // automationState 是账号任务 feature Hook 提供的表单和动作状态。
  const { form, loading, saving, running, error, summary, retryAvailable, setForm, save, run, retry } = useAccountAutomation({ account, onSaved });

  return (
    <div className="modal-overlay-centered">
      <div className="modal-container" style={{ maxWidth: '640px' }} role="dialog" aria-modal="true" aria-labelledby="account-task-title">
        <div className="modal-header">
          <div>
            <h3 id="account-task-title" className="text-2xl font-extrabold text-slate-950">账号自动任务</h3>
            <p className="mt-1 text-sm text-slate-500">{account.nickname || account.remark || account.id}</p>
          </div>
          <button type="button" onClick={onClose} className="rounded-xl p-2 text-slate-400 hover:bg-slate-100 hover:text-slate-700" aria-label="关闭">
            <X className="h-5 w-5" />
          </button>
        </div>

        <div className="modal-body space-y-5">
          {loading && <div className="rounded-xl bg-slate-50 px-4 py-3 text-sm text-slate-500">正在读取任务设置...</div>}
          <section className="rounded-2xl border border-slate-200 bg-white p-5">
            <div className="flex items-start gap-4">
              <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-emerald-50 text-emerald-600"><MessageSquareQuote className="h-5 w-5" /></div>
              <div className="min-w-0 flex-1">
                <div className="flex items-center justify-between gap-4">
                  <div>
                    <h4 className="font-black text-slate-900">自动评价</h4>
                    <p className="mt-1 text-xs leading-5 text-slate-500">持续扫描待评价订单，按订单执行；不是每日任务。</p>
                  </div>
                  <Toggle checked={form.auto_rate_enabled} onChange={/* 当前回调处理用户交互或异步状态变化。 */ () => setForm(/* 当前回调处理用户交互或异步状态变化。 */ current => ({ ...current, auto_rate_enabled: !current.auto_rate_enabled }))} label="自动评价" />
                </div>
                <label className="mt-4 block text-xs font-extrabold text-slate-600">统一好评文案</label>
                <textarea aria-label="统一好评文案" value={form.rate_content} maxLength={500} onChange={/* 当前回调处理用户交互或异步状态变化。 */ event => setForm(/* 当前回调处理用户交互或异步状态变化。 */ current => ({ ...current, rate_content: event.target.value }))}
                  className="mt-2 h-24 w-full resize-none rounded-xl border border-slate-200 bg-slate-50 px-3 py-2.5 text-sm leading-6 text-slate-800 placeholder:text-slate-400 outline-none focus:border-sky-400 focus:ring-2 focus:ring-sky-100" />
                <div className="mt-3 flex items-center justify-between gap-3">
                  <span className="text-xs text-slate-400">最近扫描：{form.last_rate_scan_at ? new Date(form.last_rate_scan_at * 1000).toLocaleString('zh-CN') : '尚未执行'}</span>
                  <button type="button" disabled={running !== '' || !account.enabled} onClick={/* 当前回调处理用户交互或异步状态变化。 */ () => void run('auto_rate')}
                    className="flex items-center gap-2 rounded-xl bg-emerald-50 px-3 py-2 text-xs font-extrabold text-emerald-700 hover:bg-emerald-100 disabled:opacity-50">
                    {running === 'auto_rate' ? <Loader2 className="h-4 w-4 animate-spin" /> : <Play className="h-4 w-4" />}立即评价
                  </button>
                </div>
              </div>
            </div>
          </section>

          <section className="rounded-2xl border border-slate-200 bg-white p-5">
            <div className="flex items-start gap-4">
              <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-amber-50 text-amber-600"><Sparkles className="h-5 w-5" /></div>
              <div className="min-w-0 flex-1">
                <div className="flex items-center justify-between gap-4">
                  <div>
                    <h4 className="font-black text-slate-900">每日自动擦亮</h4>
                    <p className="mt-1 text-xs leading-5 text-slate-500">每个账号每天最多执行一次，按北京时间判断。</p>
                  </div>
                  <Toggle checked={form.auto_polish_enabled} onChange={/* 当前回调处理用户交互或异步状态变化。 */ () => setForm(/* 当前回调处理用户交互或异步状态变化。 */ current => ({ ...current, auto_polish_enabled: !current.auto_polish_enabled }))} label="每日自动擦亮" />
                </div>
                <div className="mt-4 flex items-end justify-between gap-4">
                  <label className="block">
                    <span className="text-xs font-extrabold text-slate-600">每日执行时间</span>
                    <input type="time" value={form.polish_time} onChange={/* 当前回调处理用户交互或异步状态变化。 */ event => setForm(/* 当前回调处理用户交互或异步状态变化。 */ current => ({ ...current, polish_time: event.target.value }))}
                      className="mt-2 block rounded-xl border border-slate-200 bg-slate-50 px-3 py-2 text-sm font-bold text-slate-800 outline-none focus:border-sky-400" />
                  </label>
                  <button type="button" disabled={running !== '' || !account.enabled} onClick={/* 当前回调处理用户交互或异步状态变化。 */ () => void run('auto_polish')}
                    className="flex items-center gap-2 rounded-xl bg-amber-50 px-3 py-2 text-xs font-extrabold text-amber-700 hover:bg-amber-100 disabled:opacity-50">
                    {running === 'auto_polish' ? <Loader2 className="h-4 w-4 animate-spin" /> : <CalendarClock className="h-4 w-4" />}立即擦亮
                  </button>
                </div>
                <div className="mt-3 text-xs text-slate-400">上次完成：{form.last_polish_at ? new Date(form.last_polish_at * 1000).toLocaleString('zh-CN') : '尚未执行'}</div>
              </div>
            </div>
          </section>

          <SkipPinSection accountId={account.id} />

          {summary && (
            <div className="rounded-xl border border-sky-100 bg-sky-50 px-4 py-3 text-sm text-sky-900">
              本次发现 {summary.found} 项，成功 {summary.success}，失败 {summary.failed}，跳过 {summary.skipped}。{summary.message || ''}
            </div>
          )}
          {error && <div className="flex items-center justify-between gap-3 rounded-xl border border-red-100 bg-red-50 px-4 py-3 text-sm font-medium text-red-700"><span>{error}</span>{retryAvailable && <button type="button" className="font-bold underline" onClick={/* 当前回调处理用户交互或异步状态变化。 */ () => void retry()}>重试</button>}</div>}
        </div>

        <div className="modal-footer">
          <div className="flex w-full gap-3">
            <button type="button" onClick={onClose} disabled={saving || running !== ''}
              className="flex-1 rounded-xl bg-gray-100 px-6 py-3 font-bold text-gray-700 transition-colors hover:bg-gray-200 disabled:opacity-50">关闭</button>
            <button type="button" onClick={/* 当前回调处理用户交互或异步状态变化。 */ () => void save()} disabled={saving || running !== '' || (form.auto_rate_enabled && !form.rate_content.trim())}
              className="ios-btn-primary flex flex-1 items-center justify-center gap-2 rounded-xl px-6 py-3 font-bold disabled:opacity-50">
              {saving ? <Loader2 className="h-4 w-4 animate-spin" /> : <Save className="h-4 w-4" />}保存
            </button>
          </div>
        </div>
      </div>
    </div>
  );
};

export default AccountAutomationModal;
