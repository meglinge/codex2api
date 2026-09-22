import { useCallback, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link2, Plus, Star, Trash2 } from "lucide-react";

import { api } from "../api";
import PageHeader from "../components/PageHeader";
import StateShell from "../components/StateShell";
import { useDataLoader } from "../hooks/useDataLoader";
import { useToast } from "../hooks/useToast";
import type { CodexUpstream, CodexUpstreamsSettings } from "../types";
import { getErrorMessage } from "../utils/error";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { cn } from "@/lib/utils";

const OFFICIAL_ID = "";

function newUpstreamID(): string {
  return `up_${Date.now().toString(36)}`;
}

function validateBaseURL(value: string): boolean {
  const trimmed = value.trim().replace(/\/+$/, "");
  if (!trimmed) return false;
  try {
    const parsed = new URL(trimmed);
    return (parsed.protocol === "https:" || parsed.protocol === "http:") && Boolean(parsed.host);
  } catch {
    return false;
  }
}

export default function CodexUpstreams() {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [saving, setSaving] = useState(false);
  const [draft, setDraft] = useState<CodexUpstreamsSettings | null>(null);

  const { data, loading, error, reload } = useDataLoader<CodexUpstreamsSettings>({
    initialData: { default_id: "", upstreams: [], official_url: "", official_name: "" },
    load: async () => {
      const next = await api.getCodexUpstreams();
      setDraft(next);
      return next;
    },
  });

  const form = draft ?? data;

  const update = useCallback((patch: Partial<CodexUpstreamsSettings>) => {
    setDraft((current) => ({ ...(current ?? data), ...patch }));
  }, [data]);

  const updateRow = (id: string, patch: Partial<CodexUpstream>) => {
    update({
      upstreams: form.upstreams.map((row) => (row.id === id ? { ...row, ...patch } : row)),
    });
  };

  const save = async () => {
    for (const row of form.upstreams) {
      if (!row.name.trim()) {
        showToast(t("codexUpstreams.nameRequired"), "error");
        return;
      }
      if (!validateBaseURL(row.base_url)) {
        showToast(t("codexUpstreams.urlInvalid"), "error");
        return;
      }
    }
    setSaving(true);
    try {
      const saved = await api.updateCodexUpstreams({
        default_id: form.default_id,
        upstreams: form.upstreams.map((row) => ({
          ...row,
          name: row.name.trim(),
          base_url: row.base_url.trim().replace(/\/+$/, ""),
        })),
      });
      setDraft(saved);
      showToast(t("codexUpstreams.saved"));
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setSaving(false);
    }
  };

  return (
    <StateShell
      variant="page"
      loading={loading && form.upstreams.length === 0 && !form.official_url}
      error={error}
      onRetry={() => void reload()}
    >
      <PageHeader
        title={t("codexUpstreams.title")}
        description={t("codexUpstreams.description")}
        onRefresh={() => void reload()}
        actions={
          <Button onClick={() => void save()} disabled={saving}>
            {saving ? t("common.loading") : t("common.save")}
          </Button>
        }
      />
      <div className="mt-4 space-y-3">
        <UpstreamCard
          name={form.official_name || t("codexUpstreams.official")}
          url={form.official_url}
          isDefault={form.default_id === OFFICIAL_ID}
          locked
          onMakeDefault={() => update({ default_id: OFFICIAL_ID })}
        />
        {form.upstreams.map((row) => (
          <UpstreamCard
            key={row.id}
            name={row.name}
            url={row.base_url}
            enabled={row.enabled}
            isDefault={form.default_id === row.id}
            onName={(name) => updateRow(row.id, { name })}
            onURL={(base_url) => updateRow(row.id, { base_url })}
            onEnabled={(enabled) => {
              updateRow(row.id, { enabled });
              if (!enabled && form.default_id === row.id) update({ default_id: OFFICIAL_ID });
            }}
            onMakeDefault={() => {
              if (!row.enabled) return;
              update({ default_id: row.id });
            }}
            onDelete={() => {
              update({
                upstreams: form.upstreams.filter((item) => item.id !== row.id),
                default_id: form.default_id === row.id ? OFFICIAL_ID : form.default_id,
              });
            }}
          />
        ))}
        <Button
          type="button"
          variant="outline"
          onClick={() =>
            update({
              upstreams: [
                ...form.upstreams,
                { id: newUpstreamID(), name: "", base_url: "https://", enabled: true },
              ],
            })
          }
        >
          <Plus className="size-4" />
          {t("codexUpstreams.add")}
        </Button>
      </div>
    </StateShell>
  );
}

function UpstreamCard({
  name,
  url,
  enabled = true,
  isDefault,
  locked = false,
  onName,
  onURL,
  onEnabled,
  onMakeDefault,
  onDelete,
}: {
  name: string;
  url: string;
  enabled?: boolean;
  isDefault: boolean;
  locked?: boolean;
  onName?: (value: string) => void;
  onURL?: (value: string) => void;
  onEnabled?: (value: boolean) => void;
  onMakeDefault: () => void;
  onDelete?: () => void;
}) {
  const { t } = useTranslation();
  return (
    <Card className={cn(!enabled && "opacity-60")}>
      <CardContent className="flex flex-col gap-3 p-4 sm:flex-row sm:items-center">
        <div className="flex min-w-0 flex-1 flex-col gap-2">
          <div className="flex items-center gap-2">
            <Link2 className="size-4 shrink-0 text-muted-foreground" />
            {locked ? (
              <span className="font-semibold">{name}</span>
            ) : (
              <Input value={name} onChange={(event) => onName?.(event.target.value)} placeholder={t("codexUpstreams.namePlaceholder")} />
            )}
            {isDefault ? (
              <span className="inline-flex items-center gap-1 rounded-full bg-primary/10 px-2 py-0.5 text-xs font-semibold text-primary">
                <Star className="size-3" />
                {t("codexUpstreams.default")}
              </span>
            ) : null}
          </div>
          {locked ? (
            <code className="truncate text-xs text-muted-foreground">{url}</code>
          ) : (
            <Input value={url} onChange={(event) => onURL?.(event.target.value)} placeholder="https://example.com/backend-api/codex" />
          )}
        </div>
        <div className="flex items-center gap-2">
          {!locked ? <Switch checked={enabled} onCheckedChange={onEnabled} /> : null}
          <Button type="button" variant={isDefault ? "secondary" : "outline"} size="sm" disabled={!enabled || isDefault} onClick={onMakeDefault}>
            {t("codexUpstreams.makeDefault")}
          </Button>
          {!locked ? (
            <Button type="button" variant="ghost" size="icon" onClick={onDelete} aria-label={t("common.delete")}>
              <Trash2 className="size-4" />
            </Button>
          ) : null}
        </div>
      </CardContent>
    </Card>
  );
}
