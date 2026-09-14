// 账号测连弹窗:Codex、Claude(经 Codex 诊断帧)、Grok/Responses、Antigravity 共用。
// 消费 GET /api/admin/accounts/:id/test 的 SSE:test_start → content* → diagnostics /
// test_complete / error;codex_diagnostics 可能挂在任意事件上,读到 SSE 关闭再刷新列表。
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  Activity,
  CheckCircle,
  ChevronDown,
  Copy,
  Gauge,
  Loader2,
  Radio,
  RefreshCw,
  RotateCcw,
  XCircle,
} from "lucide-react";
import { api, getAdminKey, type ProxyRow } from "../api";
import type { AccountRow, CodexTurnStateHealth, CodexTurnStateInfo } from "../types";
import type { CodexTestDiagnostics, CodexTestWindow } from "../lib/codexConnectionTest";
import {
  clampCodexTestPercent,
  codexTestTokenMetrics,
  codexTestWindowKind,
  codexTurnStateTTL,
  codexTurnStateVerdict,
  extractCodexTurnState,
  formatCodexTestMS,
  formatCodexTestReset,
  isFinalCodexTestDiagnostics,
} from "../lib/codexConnectionTest";
import {
  DEFAULT_TEST_MODEL,
  exactModelMappingAliases,
  extractTextModels,
  formatAccountName,
  formatTestErrorMessage,
  isConnectionTestModel,
  uniqueTestModels,
} from "../lib/connectionTestModels";
import { orderAntigravityTestModels } from "../lib/antigravityModels";
import { cn } from "@/lib/utils";
import { useToast } from "../hooks/useToast";
import Modal from "./Modal";
import { ProxyField } from "./ProxyField";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";

async function copyTextToClipboard(text: string) {
  if (window.isSecureContext && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return;
    } catch {
      // Fall back for browsers that block clipboard writes.
    }
  }
  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "true");
  textarea.style.position = "fixed";
  textarea.style.top = "0";
  textarea.style.left = "0";
  textarea.style.opacity = "0";
  document.body.appendChild(textarea);
  textarea.select();
  try {
    document.execCommand("copy");
  } finally {
    document.body.removeChild(textarea);
  }
}

// 降智徽章:只信后端给的 turn_state_health;没有校验结果时显示"保存后校验"。
function TurnStateHealthBadge({ health }: { health?: CodexTurnStateHealth | null }) {
  const { t } = useTranslation();
  if (!health) {
    return (
      <Badge variant="outline" className="text-[10px] font-medium text-muted-foreground">
        {t("accounts.testTurnStatePendingCheck")}
      </Badge>
    );
  }
  const verdict = codexTurnStateVerdict(health);
  if (verdict === "healthy") {
    return (
      <Badge
        variant="outline"
        className="border-emerald-300 bg-emerald-50 text-[10px] font-medium text-emerald-700 dark:border-emerald-900/60 dark:bg-emerald-950/40 dark:text-emerald-400"
      >
        {t("accounts.testTurnStateHealthy", { len: health.cipher_len })}
      </Badge>
    );
  }
  if (verdict === "degraded") {
    return (
      <Badge
        variant="outline"
        className="border-red-300 bg-red-50 text-[10px] font-medium text-red-700 dark:border-red-900/60 dark:bg-red-950/40 dark:text-red-400"
      >
        {t("accounts.testTurnStateDegraded", { len: health.cipher_len, expected: health.expected_cipher_len })}
      </Badge>
    );
  }
  return (
    <Badge
      variant="outline"
      className="border-amber-300 bg-amber-50 text-[10px] font-medium text-amber-700 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-400"
      title={health.error}
    >
      {t("accounts.testTurnStateInvalid")}
    </Badge>
  );
}

function formatTurnStateClock(iso?: string): string {
  if (!iso) return "";
  const ts = Date.parse(iso);
  if (!Number.isFinite(ts)) return "";
  return new Date(ts).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

// TTL 行:自动缓存模型显示倒计时/已过期;其它模型说明不按 TTL 过期。
function TurnStateTTLLine({ info, now }: { info?: CodexTurnStateInfo; now: number }) {
  const { t } = useTranslation();
  if (!info) return null;
  const ttl = codexTurnStateTTL(info, now);
  const captured = formatTurnStateClock(info.captured_at);
  const text =
    ttl.state === "active"
      ? t("accounts.testTurnStateTTLActive", {
          time: formatCodexTestReset(ttl.remainingSeconds),
          at: formatTurnStateClock(info.expires_at),
        })
      : ttl.state === "expired"
        ? t("accounts.testTurnStateTTLExpired")
        : t("accounts.testTurnStateTTLNotCached");
  return (
    <span
      className={cn(
        "text-[11px] tabular-nums",
        ttl.state === "expired" ? "text-amber-600 dark:text-amber-400" : "text-muted-foreground",
      )}
    >
      {text}
      {captured ? ` · ${t("accounts.testTurnStateCapturedAt", { at: captured })}` : ""}
    </span>
  );
}

interface TurnStateInfoSnapshot {
  values: Record<string, string>;
  info: Record<string, CodexTurnStateInfo>;
}

function snapshotTurnStateInfo(account: AccountRow): TurnStateInfoSnapshot {
  return {
    values: { ...(account.codex_turn_states ?? {}) },
    info: { ...(account.codex_turn_state_info ?? {}) },
  };
}

// 只有输入框里的值与后端校验时的值一致,校验结果才算数。
function turnStateInfoFor(snapshot: TurnStateInfoSnapshot, model: string, value: string): CodexTurnStateInfo | undefined {
  if (!value || (snapshot.values[model] ?? "").trim() !== value) return undefined;
  return snapshot.info[model];
}

interface TestEvent {
  type: "test_start" | "content" | "diagnostics" | "test_complete" | "error";
  text?: string;
  model?: string;
  success?: boolean;
  error?: string;
  // Codex/Responses 测连诊断:首帧只有响应头信息,流结束后的最终帧带 duration_ms,
  // 且可能出现在 test_complete/error 之后,因此要读到 SSE 关闭再刷新列表。
  codex_diagnostics?: CodexTestDiagnostics;
}

export default function TestConnectionModal({
  account,
  onClose,
  onSettled,
  successHint,
  restoreOnSuccess,
}: {
  account: AccountRow;
  onClose: () => void;
  onSettled: () => void;
  successHint?: string;
  restoreOnSuccess?: boolean;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [output, setOutput] = useState<string[]>([]);
  const [status, setStatus] = useState<
    "idle" | "connecting" | "streaming" | "success" | "error"
  >("idle");
  const [errorMsg, setErrorMsg] = useState("");
  const [model, setModel] = useState("");
  const [selectedModel, setSelectedModel] = useState("");
  const [modelOptions, setModelOptions] = useState<string[]>([]);
  const [modelOptionsReady, setModelOptionsReady] = useState(false);
  const [diagnostics, setDiagnostics] = useState<CodexTestDiagnostics | null>(null);
  const [detailsOpen, setDetailsOpen] = useState(false);
  const [headersOpen, setHeadersOpen] = useState(false);
  const [rawOpen, setRawOpen] = useState(false);
  const [proxyUrl, setProxyUrl] = useState("");
  const [proxyPool, setProxyPool] = useState<ProxyRow[]>([]);
  const [turnStates, setTurnStates] = useState<Record<string, string>>(
    () => ({ ...(account.codex_turn_states ?? {}) }),
  );
  // 每个模型保存值的降智校验与 TTL,来自账号详情;保存成功后重新拉一次账号刷新。
  // 连同校验时的值一起存,输入框里改了还没保存的值不会套用旧结论。
  const [turnStateInfo, setTurnStateInfo] = useState<TurnStateInfoSnapshot>(() =>
    snapshotTurnStateInfo(account),
  );
  const [now, setNow] = useState(() => Date.now());
  const [pingTurnState, setPingTurnState] = useState("");
  const [turnStateChanged, setTurnStateChanged] = useState(false);
  const persistTurnStatesRef = useRef(turnStates);
  persistTurnStatesRef.current = turnStates;
  const persistTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  const outputEndRef = useRef<HTMLDivElement>(null);
  const settledRef = useRef(false);
  const onSettledRef = useRef(onSettled);
  onSettledRef.current = onSettled;

  const markSettled = useCallback(() => {
    if (settledRef.current) return;
    settledRef.current = true;
    onSettledRef.current();
  }, []);

  const isClaudeAccount = Boolean(account.claude_api);
  // Antigravity 账号行携带的 models 已是对外发布的固定档位 ID,默认模型取系统设置里
  // 该渠道的测试模型,否则取版本最新的 flash 低档(目录里会残留已下线旧版)。
  const isAntigravityAccount = Boolean(account.antigravity_api);
  // Grok 与 openai_responses 同属"账号自带模型清单"的 relay 风格账号，
  // Claude 也使用账号级原生 Messages 模型清单，但走独立分支。
  const isOpenAIResponsesAccount = Boolean(
    account.openai_responses_api || account.grok_api,
  );
  const isCodexAccount =
    !isClaudeAccount && !isAntigravityAccount && !isOpenAIResponsesAccount;
  const canPersistTurnStates = isCodexAccount && account.status !== "deleted";

  const refreshTurnStateInfo = useCallback(async () => {
    try {
      const fresh = await api.getAccount(account.id);
      setTurnStateInfo(snapshotTurnStateInfo(fresh));
      setNow(Date.now());
    } catch {
      /* 拉不到就保留旧状态,不打断保存流程 */
    }
  }, [account.id]);

  const persistTurnStates = useCallback(
    (next: Record<string, string>, immediate = false) => {
      setTurnStates(next);
      persistTurnStatesRef.current = next;
      if (!canPersistTurnStates) return;
      const save = () => {
        void api
          .updateAccountCodexTurnStates(account.id, next)
          .then(() => refreshTurnStateInfo())
          .catch(() => {
            showToast(t("accounts.testTurnStateSaveFailed"), "error");
          });
      };
      if (persistTimerRef.current) {
        clearTimeout(persistTimerRef.current);
        persistTimerRef.current = null;
      }
      if (immediate) {
        save();
        return;
      }
      persistTimerRef.current = setTimeout(save, 400);
    },
    [account.id, canPersistTurnStates, refreshTurnStateInfo, showToast, t],
  );

  const persistTurnState = useCallback(
    (model: string, value: string, immediate = true) => {
      if (!model) return;
      persistTurnStates({ ...persistTurnStatesRef.current, [model]: value }, immediate);
    },
    [persistTurnStates],
  );

  const modelSelectOptions = useMemo(
    () =>
      uniqueTestModels(
        modelOptions,
        selectedModel,
        !isOpenAIResponsesAccount && !isClaudeAccount && !isAntigravityAccount,
      ).map((item) => ({ label: item, value: item })),
    [isAntigravityAccount, isClaudeAccount, isOpenAIResponsesAccount, modelOptions, selectedModel],
  );

  useEffect(() => {
    setTurnStates({ ...(account.codex_turn_states ?? {}) });
  }, [account.id, account.codex_turn_states]);

  useEffect(() => {
    setTurnStateInfo({
      values: { ...(account.codex_turn_states ?? {}) },
      info: { ...(account.codex_turn_state_info ?? {}) },
    });
  }, [account.id, account.codex_turn_state_info, account.codex_turn_states]);

  // 剩余 TTL 倒计时:每 30 秒推进一次时钟,弹窗开着也能看到到期。
  useEffect(() => {
    if (!isCodexAccount) return;
    const timer = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(timer);
  }, [isCodexAccount]);

  useEffect(() => {
    if (!isCodexAccount) return;
    let active = true;
    void api
      .listProxies()
      .then((res) => {
        if (active) setProxyPool(res.proxies ?? []);
      })
      .catch(() => {
        if (active) setProxyPool([]);
      });
    return () => {
      active = false;
    };
  }, [isCodexAccount]);

  useEffect(() => {
    let active = true;

    const loadModels = async () => {
      try {
        if (isAntigravityAccount) {
          let preferred = "";
          try {
            preferred = (await api.getChannelTestSettings()).antigravity.test_model ?? "";
          } catch {
            /* 渠道测试设置读不到就按目录自动选 */
          }
          if (!active) return;
          const ordered = orderAntigravityTestModels(account.models ?? [], preferred);
          setModelOptions(ordered);
          setSelectedModel((current) => current || ordered[0] || "");
          return;
        }

        const settings = await api.getSettings();
        if (!active) return;

        if (isClaudeAccount) {
          const accountModels = (account.models ?? []).filter(
            (model) => isConnectionTestModel(model) && model.toLowerCase().startsWith("claude-"),
          );
          const fallbackModels = uniqueTestModels(
            accountModels.length > 0 ? accountModels : ["claude-opus-4-5", "claude-sonnet-4-5", "claude-haiku-4-5"],
            undefined,
            false,
          );
          setModelOptions(fallbackModels);
          setSelectedModel((current) => current || fallbackModels[0] || "");
          return;
        }

        if (isOpenAIResponsesAccount) {
          const accountModels = (account.models ?? []).filter(
            isConnectionTestModel,
          );
          const mappingAliases = exactModelMappingAliases(
            account.model_mapping,
            accountModels,
          );
          const testModels = uniqueTestModels(
            [...mappingAliases, ...accountModels],
            undefined,
            false,
          );
          const preferredModel =
            testModels.find(
              (item) =>
                item.toLowerCase() === settings.test_model.toLowerCase(),
            ) ??
            mappingAliases[0] ??
            accountModels[0];
          const nextModels = uniqueTestModels(
            testModels,
            preferredModel,
            false,
          );
          setModelOptions(nextModels);
          setSelectedModel((current) => current || nextModels[0] || "");
          return;
        }

        const modelsResp = await api.getModels();
        if (!active) return;
        const upstreamModels = extractTextModels(modelsResp);
        const preferredModel = isConnectionTestModel(settings.test_model)
          ? settings.test_model
          : DEFAULT_TEST_MODEL;
        const nextModels = uniqueTestModels(upstreamModels, preferredModel);
        setModelOptions(nextModels);
        setSelectedModel(
          (current) => current || nextModels[0] || DEFAULT_TEST_MODEL,
        );
      } catch {
        if (!active) return;
        if (isAntigravityAccount) {
          const ordered = orderAntigravityTestModels(account.models ?? [], "");
          setModelOptions(ordered);
          setSelectedModel((current) => current || ordered[0] || "");
        } else if (isClaudeAccount) {
          const accountModels = (account.models ?? []).filter(
            (model) => isConnectionTestModel(model) && model.toLowerCase().startsWith("claude-"),
          );
          const fallbackModels = uniqueTestModels(
            accountModels.length > 0 ? accountModels : ["claude-opus-4-5", "claude-sonnet-4-5", "claude-haiku-4-5"],
            undefined,
            false,
          );
          setModelOptions(fallbackModels);
          setSelectedModel((current) => current || fallbackModels[0] || "");
        } else if (isOpenAIResponsesAccount) {
          const accountModels = (account.models ?? []).filter(
            isConnectionTestModel,
          );
          const mappingAliases = exactModelMappingAliases(
            account.model_mapping,
            accountModels,
          );
          const fallbackModels = uniqueTestModels(
            [...mappingAliases, ...accountModels],
            undefined,
            false,
          );
          setModelOptions(fallbackModels);
          setSelectedModel((current) => current || fallbackModels[0] || "");
        } else {
          const fallbackModels = uniqueTestModels([], DEFAULT_TEST_MODEL);
          setModelOptions(fallbackModels);
          setSelectedModel((current) => current || fallbackModels[0]);
        }
      } finally {
        if (active) {
          setModelOptionsReady(true);
        }
      }
    };

    void loadModels();

    return () => {
      active = false;
    };
  }, [account.claude_api, account.model_mapping, account.models, isAntigravityAccount, isClaudeAccount, isOpenAIResponsesAccount]);

  const runConnectionTest = useCallback(
    async (options?: { replayTurnState?: boolean }) => {
      if (!selectedModel) return;

      abortRef.current?.abort();
      const controller = new AbortController();
      abortRef.current = controller;

      setOutput([]);
      setStatus("connecting");
      setErrorMsg("");
      setDiagnostics(null);
      setPingTurnState("");
      setTurnStateChanged(false);
      setModel(selectedModel);
      settledRef.current = false;

      const replayTurnState = Boolean(options?.replayTurnState);
      const sentTurnState = replayTurnState
        ? (turnStates[selectedModel] ?? "").trim()
        : "";

      try {
        const params = new URLSearchParams({ model: selectedModel });
        if (restoreOnSuccess) {
          params.set("restore_on_success", "true");
        }
        const trimmedProxy = proxyUrl.trim();
        if (trimmedProxy) {
          params.set("proxy_url", trimmedProxy);
        }
        if (sentTurnState) {
          params.set("turn_state", sentTurnState);
        }
        const res = await fetch(
          `/api/admin/accounts/${account.id}/test?${params.toString()}`,
          {
            signal: controller.signal,
            headers: getAdminKey() ? { "X-Admin-Key": getAdminKey() } : {},
          },
        );

        if (!res.ok) {
          const body = await res.text();
          let msg = `HTTP ${res.status}`;
          try {
            const parsed = JSON.parse(body);
            if (parsed.error) msg = parsed.error;
          } catch {
            /* ignore */
          }
          setStatus("error");
          setErrorMsg(msg);
          markSettled();
          return;
        }

        const reader = res.body?.getReader();
        if (!reader) {
          setStatus("error");
          setErrorMsg(t("accounts.browserStreamingUnsupported"));
          markSettled();
          return;
        }

        const decoder = new TextDecoder();
        let buffer = "";
        let receivedTerminalEvent = false;
        let latestDiagnostics: CodexTestDiagnostics | null = null;

        const processEventLines = (lines: string[]) => {
          for (const line of lines) {
            const trimmed = line.trim();
            if (!trimmed.startsWith("data: ")) continue;

            try {
              const event: TestEvent = JSON.parse(trimmed.slice(6));
              // 请求阶段失败时后端不单发 diagnostics 帧,而是把诊断挂在 error 事件上,
              // 因此不分事件类型,带了就收。
              if (event.codex_diagnostics) {
                latestDiagnostics = event.codex_diagnostics;
                setDiagnostics(event.codex_diagnostics);
              }

              switch (event.type) {
                case "test_start":
                  setModel(event.model || selectedModel);
                  setStatus("streaming");
                  break;
                case "content":
                  if (event.text) {
                    setOutput((prev) => [...prev, event.text!]);
                  }
                  break;
                case "diagnostics":
                  break;
                case "test_complete":
                  receivedTerminalEvent = true;
                  setStatus(event.success ? "success" : "error");
                  break;
                case "error":
                  receivedTerminalEvent = true;
                  setStatus("error");
                  setErrorMsg(event.error || t("accounts.unknownError"));
                  break;
              }
            } catch {
              /* ignore non-JSON lines */
            }
          }
        };

        while (true) {
          const { done, value } = await reader.read();
          if (done) {
            buffer += decoder.decode();
            break;
          }

          buffer += decoder.decode(value, { stream: true });
          const lines = buffer.split("\n");
          buffer = lines.pop() || "";
          processEventLines(lines);
        }

        if (buffer.trim()) {
          processEventLines([buffer]);
        }

        const observedTurnState = extractCodexTurnState(latestDiagnostics);
        if (observedTurnState) {
          setPingTurnState(observedTurnState);
          if (!replayTurnState) {
            persistTurnState(selectedModel, observedTurnState);
          } else if (sentTurnState && sentTurnState !== observedTurnState) {
            setTurnStateChanged(true);
          }
        }

        if (receivedTerminalEvent) {
          // 等服务端关闭 SSE 后再刷新列表：后端会在连接结束时提交状态并失效
          // 账号快照，提前刷新会重新读到“未采样”的旧缓存。
          markSettled();
        } else {
          setStatus("error");
          setErrorMsg(t("accounts.connectionEndedUnexpectedly"));
          markSettled();
        }
      } catch (err: unknown) {
        if (err instanceof DOMException && err.name === "AbortError") return;
        setStatus("error");
        setErrorMsg(
          err instanceof Error ? err.message : t("accounts.connectionFailed"),
        );
        markSettled();
      }
    },
    [
      account.id,
      markSettled,
      persistTurnState,
      proxyUrl,
      restoreOnSuccess,
      selectedModel,
      t,
      turnStates,
    ],
  );

  useEffect(() => {
    return () => {
      abortRef.current?.abort();
      if (persistTimerRef.current) {
        clearTimeout(persistTimerRef.current);
      }
    };
  }, []);

  useEffect(() => {
    outputEndRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [output]);

  const statusText = {
    idle: t("accounts.testIdle"),
    connecting: t("accounts.connecting"),
    streaming: t("accounts.receivingResponse"),
    success: t("accounts.testSuccess"),
    error: t("accounts.testFailed"),
  }[status];
  const StatusIcon = {
    idle: Radio,
    connecting: Loader2,
    streaming: Loader2,
    success: CheckCircle,
    error: XCircle,
  }[status];
  const statusIconSpin = status === "connecting" || status === "streaming";

  const statusColor = {
    idle: "text-muted-foreground",
    connecting: "text-muted-foreground",
    streaming: "text-blue-500",
    success: "text-emerald-500",
    error: "text-red-500",
  }[status];
  const formattedErrorMsg = errorMsg ? formatTestErrorMessage(errorMsg) : "";
  const handleCopyFailureDetails = async () => {
    try {
      await copyTextToClipboard(formattedErrorMsg);
      showToast(t("common.copied"));
    } catch {
      showToast(t("common.copyFailed"), "error");
    }
  };
  const running = status === "connecting" || status === "streaming";
  const selectedTurnState = turnStates[selectedModel] ?? "";
  const applyPingTurnState = () => {
    if (!pingTurnState || !selectedModel) return;
    persistTurnState(selectedModel, pingTurnState);
  };
  const handleCopyTurnState = async (value: string) => {
    try {
      await copyTextToClipboard(value);
      showToast(t("common.copied"));
    } catch {
      showToast(t("common.copyFailed"), "error");
    }
  };
  const diagnosticsFinal = isFinalCodexTestDiagnostics(diagnostics);
  const handleCopyDiagnostics = async () => {
    try {
      await copyTextToClipboard(
        JSON.stringify(
          {
            status,
            model,
            error: errorMsg || undefined,
            output: output.join(""),
            diagnostics,
          },
          null,
          2,
        ),
      );
      showToast(t("common.copied"));
    } catch {
      showToast(t("common.copyFailed"), "error");
    }
  };
  const timingTiles = [
    {
      key: "http",
      label: t("accounts.testDiagHTTPStatus"),
      value: diagnostics?.http_status ? String(diagnostics.http_status) : "—",
      tone:
        !diagnostics?.http_status
          ? "text-foreground"
          : diagnostics.http_status < 300
            ? "text-emerald-600 dark:text-emerald-400"
            : diagnostics.http_status === 429
              ? "text-amber-600 dark:text-amber-400"
              : "text-red-600 dark:text-red-400",
    },
    { key: "headers", label: t(diagnostics?.transport === "websocket" ? "accounts.testDiagFirstFrameTime" : "accounts.testDiagHeadersTime"), value: formatCodexTestMS(diagnostics?.transport === "websocket" ? diagnostics?.first_frame_ms : diagnostics?.headers_ms), tone: "text-foreground" },
    { key: "first", label: t("accounts.testDiagFirstContent"), value: formatCodexTestMS(diagnostics?.first_content_ms), tone: "text-foreground" },
    { key: "total", label: t("accounts.testDiagDuration"), value: formatCodexTestMS(diagnostics?.duration_ms), tone: "text-foreground" },
  ];
  const windowLabel = (window?: CodexTestWindow) => {
    const kind = codexTestWindowKind(window);
    return kind === "5h"
      ? t("accounts.testDiagWindow5h")
      : kind === "7d"
        ? t("accounts.testDiagWindow7d")
        : kind === "short"
          ? t("accounts.testDiagWindowShort")
          : t("accounts.testDiagWindowUnknown");
  };
  const usageWindows = [diagnostics?.primary_window, diagnostics?.secondary_window]
    .filter((window): window is CodexTestWindow => Boolean(window))
    .map((window) => ({
      label: windowLabel(window),
      percent: clampCodexTestPercent(window.used_percent),
      reset: formatCodexTestReset(window.reset_after_seconds),
    }));
  // 安全缓冲单独成行:x-codex-safety-buffering-faster-model 只是官方 CLI 的备用切换
  // 目标,混在响应头表里容易被误读成"实际回答模型"。
  const safetyBuffering = (() => {
    if (!diagnostics) return undefined;
    const enabled = diagnostics.safety_buffering_enabled;
    const faster = diagnostics.safety_buffering_faster_model;
    if (enabled === undefined && !faster && !diagnostics.safety_buffered) return undefined;
    const parts = [
      enabled === undefined ? null : enabled ? t("accounts.testDiagSafetyBufferingOn") : t("accounts.testDiagSafetyBufferingOff"),
      faster ? t("accounts.testDiagSafetyBufferingFaster", { model: faster }) : null,
      diagnostics.safety_buffered ? t("accounts.testDiagSafetyBufferingTriggered") : null,
    ].filter(Boolean);
    return parts.join(" · ");
  })();
  const identityRows: Array<{ label: string; value?: string; hint?: string }> = [
    { label: "X-Codex-Turn-State", value: extractCodexTurnState(diagnostics) || undefined },
    { label: t("accounts.testDiagProxy"), value: diagnostics ? (diagnostics.proxy_url?.trim() || t("accounts.testDiagProxyDirect")) : undefined },
    { label: t("accounts.testDiagResponseModel"), value: diagnostics?.response_model },
    { label: t("accounts.testDiagTransport"), value: diagnostics?.transport },
    { label: t("accounts.testDiagPlan"), value: diagnostics?.plan_type },
    { label: t("accounts.testDiagSafetyBuffering"), value: safetyBuffering, hint: t("accounts.testDiagSafetyBufferingHint") },
    { label: "Response ID", value: diagnostics?.response_id },
    { label: "Request ID", value: diagnostics?.request_id },
    { label: "CF-Ray", value: diagnostics?.cf_ray },
    { label: t("accounts.testDiagResponseStatus"), value: diagnostics?.response_status },
    { label: t("accounts.testDiagIncompleteReason"), value: diagnostics?.incomplete_reason },
    { label: t("accounts.testDiagErrorType"), value: diagnostics?.error_type },
    { label: t("accounts.testDiagErrorCode"), value: diagnostics?.error_code },
  ].filter((row) => row.value);
  const tokenMetrics = codexTestTokenMetrics(diagnostics?.usage);
  const tokenLabels = {
    input_tokens: t("accounts.testDiagInputTokens"),
    output_tokens: t("accounts.testDiagOutputTokens"),
    cached_input_tokens: t("accounts.testDiagCachedTokens"),
    reasoning_output_tokens: t("accounts.testDiagReasoningTokens"),
  } as const;
  const tokenBarColors = ["bg-blue-500", "bg-emerald-500", "bg-violet-500", "bg-amber-500"];
  const hasDetails = Boolean(
    identityRows.length > 0 ||
      diagnostics?.usage ||
      diagnostics?.response_headers?.length ||
      diagnostics?.response_body,
  );
  const monoStyle = { fontFamily: "var(--font-geist-mono)" } as const;

  return (
    <Modal
      show={true}
      title={t("accounts.testConnectionTitle", {
        account: formatAccountName(account),
      })}
      onClose={() => {
        abortRef.current?.abort();
        onClose();
      }}
      footer={
        <div className="flex w-full flex-wrap items-center justify-end gap-2">
          {diagnostics ? (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="mr-auto"
              disabled={running}
              onClick={() => void handleCopyDiagnostics()}
            >
              <Copy className="size-3.5" />
              {t("accounts.testDiagCopy")}
            </Button>
          ) : null}
          <Button
            variant="outline"
            onClick={() => {
              abortRef.current?.abort();
              onClose();
            }}
          >
            {t("common.close")}
          </Button>
          {isCodexAccount ? (
            <Button
              type="button"
              variant="outline"
              disabled={running || !modelOptionsReady || !selectedModel}
              onClick={() => void runConnectionTest()}
            >
              <Radio className={cn("size-3.5", running && "animate-pulse")} />
              {t("accounts.testPing")}
            </Button>
          ) : null}
          <Button
            type="button"
            disabled={running || !modelOptionsReady || !selectedModel}
            onClick={() => void runConnectionTest({ replayTurnState: isCodexAccount })}
          >
            <RefreshCw className={cn("size-3.5", running && "animate-spin")} />
            {status === "idle" ? t("accounts.testStart") : t("accounts.testDiagRetry")}
          </Button>
        </div>
      }
      contentClassName="sm:max-w-[760px]"
    >
      <div className="space-y-4">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <span
            className={`flex items-center gap-1.5 text-sm font-semibold ${statusColor}`}
          >
            <StatusIcon
              className={cn("size-4", statusIconSpin && "animate-spin")}
            />
            {statusText}
          </span>
          <Select
            className="w-52 max-w-full"
            compact
            value={selectedModel}
            onValueChange={setSelectedModel}
            options={modelSelectOptions}
            placeholder={model || t("settings.testModel")}
            disabled={!modelOptionsReady || modelSelectOptions.length === 0 || running}
          />
        </div>

        {isCodexAccount && (
          <div className="space-y-3 rounded-xl border border-border bg-muted/20 px-4 py-3">
            <ProxyField
              value={proxyUrl}
              onChange={setProxyUrl}
              proxies={proxyPool}
              label={t("accounts.testProxyOverride")}
              labelClassName="text-[11px] font-semibold"
              placeholder={t("accounts.testProxyOverridePlaceholder")}
              disabled={running}
            />
            <p className="text-[11px] leading-relaxed text-muted-foreground">
              {t("accounts.testProxyOverrideHint")}
            </p>
            <div className="space-y-1.5">
              <div className="flex items-center justify-between gap-2 text-[11px] font-semibold text-muted-foreground">
                <span className="flex items-center gap-1.5">
                  {t("accounts.testTurnStatePing")}
                  {pingTurnState ? <TurnStateHealthBadge health={diagnostics?.turn_state_health} /> : null}
                </span>
                {pingTurnState ? (
                  <div className="flex items-center gap-1">
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      className="h-7 px-2"
                      disabled={running}
                      onClick={() => void handleCopyTurnState(pingTurnState)}
                    >
                      <Copy className="size-3.5" />
                      {t("common.copy")}
                    </Button>
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      className="h-7 px-2"
                      disabled={running || !selectedModel}
                      onClick={applyPingTurnState}
                    >
                      {t("accounts.testTurnStateApply")}
                    </Button>
                  </div>
                ) : null}
              </div>
              <pre
                className="max-h-20 overflow-auto rounded-md border border-border/70 bg-background px-2.5 py-2 text-[11px] leading-relaxed whitespace-pre-wrap break-all"
                style={monoStyle}
              >
                {pingTurnState ||
                  (selectedTurnState.trim()
                    ? t("accounts.testTurnStateSavedIdle")
                    : t("accounts.testTurnStateEmpty"))}
              </pre>
              {turnStateChanged ? (
                <p className="text-[11px] leading-relaxed text-amber-600 dark:text-amber-400">
                  {t("accounts.testTurnStateChanged")}
                </p>
              ) : null}
            </div>
            <div className="space-y-2">
              <div className="text-[11px] font-semibold text-muted-foreground">
                {t("accounts.testTurnStateByModel")}
              </div>
              <div className="max-h-56 space-y-2 overflow-auto pr-0.5">
                {modelSelectOptions.map((option) => {
                  const saved = (turnStates[option.value] ?? "").trim();
                  const info = turnStateInfoFor(turnStateInfo, option.value, saved);
                  return (
                    <div key={option.value} className="space-y-1">
                      <label className="block space-y-1">
                        <span className="block text-[11px] text-muted-foreground" style={monoStyle}>
                          {option.label}
                        </span>
                        <Input
                          value={turnStates[option.value] ?? ""}
                          onChange={(event) => {
                            persistTurnState(option.value, event.target.value, false);
                          }}
                          placeholder={t("accounts.testTurnStatePlaceholder")}
                          disabled={running}
                          className="h-8 font-mono text-[11px]"
                        />
                      </label>
                      {saved ? (
                        <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                          <TurnStateHealthBadge health={info?.health} />
                          <TurnStateTTLLine info={info} now={now} />
                        </div>
                      ) : null}
                    </div>
                  );
                })}
              </div>
              <p className="text-[11px] leading-relaxed text-muted-foreground">
                {t("accounts.testTurnStateHealthHint")}
              </p>
              {selectedTurnState ? (
                <p className="text-[11px] leading-relaxed text-muted-foreground">
                  {t("accounts.testTurnStateReplayHint")}
                </p>
              ) : (
                <p className="text-[11px] leading-relaxed text-muted-foreground">
                  {t("accounts.testTurnStatePingHint")}
                </p>
              )}
            </div>
          </div>
        )}

        {diagnostics && (
          <div className="grid grid-cols-2 gap-2 text-xs sm:grid-cols-4">
            {timingTiles.map((tile) => (
              <div key={tile.key} className="rounded-md bg-muted/40 px-2.5 py-2">
                <div className="text-[11px] text-muted-foreground">{tile.label}</div>
                <div
                  className={cn("mt-0.5 text-sm font-semibold tabular-nums", tile.tone)}
                  style={monoStyle}
                >
                  {tile.value}
                </div>
              </div>
            ))}
          </div>
        )}

        {usageWindows.length > 0 && (
          <div className="space-y-2 rounded-xl border border-border bg-muted/30 px-4 py-3">
            <div className="flex items-center justify-between gap-2 text-xs font-semibold text-muted-foreground">
              <span className="flex items-center gap-1.5">
                <Gauge className="size-3.5 text-primary" />
                {t("accounts.testDiagUsageWindows")}
              </span>
              {diagnostics?.plan_type ? (
                <Badge variant="outline" className="text-[11px] font-medium">
                  {diagnostics.plan_type}
                </Badge>
              ) : null}
            </div>
            {usageWindows.map((window, index) => (
              <div key={`${window.label}-${index}`} className="space-y-1">
                <div className="flex items-center justify-between gap-2 text-xs">
                  <span className="text-foreground">{window.label}</span>
                  <span className="tabular-nums text-muted-foreground" style={monoStyle}>
                    {window.percent === null ? "—" : `${window.percent.toFixed(window.percent >= 10 ? 0 : 1)}%`}
                    {window.reset ? ` · ${t("accounts.testDiagResetIn", { time: window.reset })}` : ""}
                  </span>
                </div>
                <div aria-hidden className="h-1.5 overflow-hidden rounded-full bg-muted">
                  <div
                    className={cn(
                      "h-full rounded-full transition-[width]",
                      window.percent !== null && window.percent >= 100
                        ? "bg-red-500"
                        : window.percent !== null && window.percent >= 80
                          ? "bg-amber-500"
                          : "bg-emerald-500",
                    )}
                    style={{ width: `${window.percent ?? 0}%` }}
                  />
                </div>
              </div>
            ))}
          </div>
        )}

        {(output.length > 0 ||
          status === "connecting" ||
          status === "streaming") && (
          <div
            className="min-h-[80px] max-h-[240px] overflow-auto rounded-lg border border-border bg-muted/30 p-3 text-[13px] leading-relaxed whitespace-pre-wrap break-all"
            style={{ fontFamily: "var(--font-geist-mono)" }}
          >
            {output.length === 0 && status === "connecting" && (
              <span className="text-muted-foreground animate-pulse">
                {t("accounts.sendingTestRequest")}
              </span>
            )}
            {output.join("")}
            <div ref={outputEndRef} />
          </div>
        )}

        {errorMsg && (
          <div className="rounded-xl border border-red-200 bg-red-50 p-3.5 text-red-600 dark:border-red-900/50 dark:bg-red-950/30 dark:text-red-400">
            <div className="mb-2 flex items-center justify-between gap-3">
              <div className="text-sm font-semibold">
                {t("accounts.failureDetails")}
              </div>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                className="h-7 shrink-0 px-2 text-red-600 hover:bg-red-100 hover:text-red-700 dark:text-red-400 dark:hover:bg-red-900/40 dark:hover:text-red-300"
                onClick={() => void handleCopyFailureDetails()}
                title={t("common.copy")}
              >
                <Copy className="size-3.5" />
                {t("common.copy")}
              </Button>
            </div>
            <pre
              className="max-h-[34vh] overflow-auto text-[13px] leading-relaxed whitespace-pre-wrap break-all"
              style={{ fontFamily: "var(--font-geist-mono)" }}
            >
              {formattedErrorMsg}
            </pre>
          </div>
        )}

        {status === "success" && (
          <div className="flex items-center gap-2 rounded-xl border border-emerald-200 bg-emerald-50 px-4 py-2.5 text-sm text-emerald-700 dark:border-emerald-900/50 dark:bg-emerald-950/30 dark:text-emerald-400">
            <RotateCcw className="size-4 shrink-0" />
            {successHint ?? t("accounts.testAutoReset")}
          </div>
        )}

        {diagnostics && hasDetails && (
          <div className="rounded-xl border border-border">
            <button
              type="button"
              className="flex w-full items-center gap-2 rounded-xl px-4 py-2.5 text-left text-xs font-semibold text-foreground hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
              aria-expanded={detailsOpen}
              onClick={() => setDetailsOpen((open) => !open)}
            >
              <Activity className="size-3.5 text-primary" />
              <span>{t("accounts.testDiagDetails")}</span>
              {running && !diagnosticsFinal && (
                <span className="text-[11px] font-normal text-muted-foreground">
                  · {t("accounts.testDiagPending")}
                </span>
              )}
              <ChevronDown
                className={cn(
                  "ml-auto size-4 shrink-0 text-muted-foreground transition-transform",
                  detailsOpen && "rotate-180",
                )}
              />
            </button>
            {detailsOpen && (
              <div className="space-y-4 border-t border-border px-4 py-3">
                {identityRows.length > 0 && (
                  <dl className="grid grid-cols-1 gap-x-4 gap-y-2 text-xs sm:grid-cols-2">
                    {identityRows.map((row) => (
                      <div key={row.label} className={cn("min-w-0", row.hint && "sm:col-span-2")}>
                        <dt className="text-[11px] text-muted-foreground">{row.label}</dt>
                        <dd className="mt-0.5 break-all text-foreground" style={monoStyle}>
                          {row.value}
                        </dd>
                        {row.hint && (
                          <p className="mt-1 text-[11px] leading-relaxed text-muted-foreground">{row.hint}</p>
                        )}
                      </div>
                    ))}
                  </dl>
                )}

                {diagnostics.usage && (
                  <div className="space-y-2">
                    <div className="flex items-center justify-between gap-2 text-xs font-semibold text-muted-foreground">
                      <span>{t("accounts.testDiagTokens")}</span>
                      {typeof diagnostics.usage.total_tokens === "number" && (
                        <span className="tabular-nums" style={monoStyle}>
                          {t("accounts.testDiagTotalTokens")} {diagnostics.usage.total_tokens.toLocaleString()}
                        </span>
                      )}
                    </div>
                    <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
                      {tokenMetrics.map((metric, index) => (
                        <div key={metric.key} className="rounded-md bg-muted/40 px-2.5 py-2">
                          <div className="text-[11px] text-muted-foreground">{tokenLabels[metric.key]}</div>
                          <div className="mt-0.5 text-sm font-semibold tabular-nums" style={monoStyle}>
                            {metric.value?.toLocaleString() ?? "—"}
                          </div>
                          <div aria-hidden className="mt-1.5 h-1 overflow-hidden rounded-full bg-muted">
                            <div
                              className={cn("h-full rounded-full transition-[width]", tokenBarColors[index])}
                              style={{ width: `${metric.percent}%` }}
                            />
                          </div>
                        </div>
                      ))}
                    </div>
                  </div>
                )}

                {diagnostics.response_headers && diagnostics.response_headers.length > 0 && (
                  <div className="rounded-lg border border-border/70">
                    <button
                      type="button"
                      className="flex w-full items-center gap-2 px-3 py-2 text-left text-xs font-medium hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
                      aria-expanded={headersOpen}
                      onClick={() => setHeadersOpen((open) => !open)}
                    >
                      <span>{t("accounts.testDiagHeaders")}</span>
                      <span className="rounded bg-muted px-1.5 text-[10px] tabular-nums text-muted-foreground">
                        {diagnostics.response_headers.length}
                      </span>
                      <ChevronDown
                        className={cn(
                          "ml-auto size-3.5 shrink-0 text-muted-foreground transition-transform",
                          headersOpen && "rotate-180",
                        )}
                      />
                    </button>
                    {headersOpen && (
                      <div className="max-h-56 overflow-auto border-t border-border/70">
                        <table className="w-full table-fixed text-left text-[11px]">
                          <tbody>
                            {diagnostics.response_headers.map((header, index) => (
                              <tr key={`${header.name}-${index}`} className="border-b border-border/40 last:border-0">
                                <th scope="row" className="w-[44%] break-all px-3 py-1.5 align-top font-normal text-muted-foreground" style={monoStyle}>
                                  {header.name}
                                </th>
                                <td className="break-all px-3 py-1.5 align-top" style={monoStyle}>
                                  {header.value}
                                </td>
                              </tr>
                            ))}
                          </tbody>
                        </table>
                      </div>
                    )}
                  </div>
                )}

                {diagnostics.response_body && (
                  <div className="rounded-lg border border-border/70">
                    <button
                      type="button"
                      className="flex w-full items-center gap-2 px-3 py-2 text-left text-xs font-medium hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
                      aria-expanded={rawOpen}
                      onClick={() => setRawOpen((open) => !open)}
                    >
                      <span>{t("accounts.testDiagRawBody")}</span>
                      {diagnostics.body_truncated && (
                        <span className="text-[10px] font-normal text-muted-foreground">
                          · {t("accounts.testDiagBodyTruncated")}
                        </span>
                      )}
                      <ChevronDown
                        className={cn(
                          "ml-auto size-3.5 shrink-0 text-muted-foreground transition-transform",
                          rawOpen && "rotate-180",
                        )}
                      />
                    </button>
                    {rawOpen && (
                      <div className="border-t border-border/70 px-3 py-2.5">
                        <p className="mb-2 text-[11px] leading-relaxed text-muted-foreground">
                          {t("accounts.testDiagRawHint")}
                        </p>
                        <pre
                          className="max-h-64 overflow-auto whitespace-pre-wrap break-all text-[11px] leading-relaxed"
                          style={monoStyle}
                        >
                          {diagnostics.response_body}
                        </pre>
                      </div>
                    )}
                  </div>
                )}
              </div>
            )}
          </div>
        )}
      </div>
    </Modal>
  );
}
