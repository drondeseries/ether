import { Button, Input, SearchSelect, SettingsSection } from "@braedonsaunders/appkit-ui";
import { Plus, Trash2 } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { MoreOptions } from "../../components/ConfigSection";
import { TestButton, type TestState } from "../../components/fields";
import { SettingsRow } from "../../components/SettingsRow";
import { t } from "../../i18n";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { api, errorMessage } from "../../lib/api";
import type { Indexer, IndexerRequestIdentity, KVOverride, RequestPreview } from "../../lib/types";

const PROFILE_IDS = [
  "ether",
  "prowlarr",
  "sonarr",
  "radarr",
  "nzbhydra2",
  "sabnzbd",
  "nzbget",
  "browser",
  "custom",
] as const;

function profileLabel(id: string): string {
  switch (id) {
    case "":
      return t("Shared setting (Ether by default)");
    case "ether":
      return t("Look like Ether");
    case "prowlarr":
      return t("Look like Prowlarr");
    case "sonarr":
      return t("Look like Sonarr");
    case "radarr":
      return t("Look like Radarr");
    case "nzbhydra2":
      return t("Look like NZBHydra2");
    case "sabnzbd":
      return t("Look like SABnzbd");
    case "nzbget":
      return t("Look like NZBGet");
    case "browser":
      return t("Look like a web browser");
    case "custom":
      return t("Custom (set everything yourself)");
    default:
      return id;
  }
}

/** The identity a draft effectively has, folding in the old user-agent box. */
export function effectiveIdentity(draft: Indexer): IndexerRequestIdentity {
  if (draft.requestIdentity) return draft.requestIdentity;
  if (draft.userAgent?.trim()) {
    return {
      profile: "custom",
      headers: [{ name: "User-Agent", value: draft.userAgent.trim() }],
    };
  }
  return { profile: "" };
}

/** The user agent mirrored into the legacy field for a saved identity. */
export function mirroredUserAgent(
  ri: IndexerRequestIdentity,
  profiles: RequestPreview["profiles"] | undefined,
): string {
  if (!ri.profile) return "";
  const ua = ri.headers?.find((h) => h.name.toLowerCase() === "user-agent" && !h.remove)?.value;
  if (ua) return ua;
  return profiles?.find((p) => p.id === ri.profile)?.userAgent ?? "";
}

const HEADER_NAME = /^[A-Za-z0-9!#$%&'*+.^_`|~-]+$/;

function overrideProblem(ovs: KVOverride[], kind: "header" | "param"): string | undefined {
  const seen = new Set<string>();
  for (const ov of ovs) {
    const name = ov.name.trim();
    if (!name) return t("Every row needs a name.");
    if (kind === "header" && !HEADER_NAME.test(name)) {
      return t("{name} is not a valid header name.", { name });
    }
    if (kind === "param" && ov.remove && (name === "apikey" || name === "t")) {
      return t("{name} cannot be removed.", { name });
    }
    const key = kind === "header" ? name.toLowerCase() : name;
    if (seen.has(key)) return t("{name} is listed twice.", { name });
    seen.add(key);
  }
  return undefined;
}

function OverrideRows({
  title,
  help,
  value,
  onChange,
  kind,
  namePlaceholder,
}: {
  title: string;
  help: string;
  value: KVOverride[];
  onChange: (v: KVOverride[]) => void;
  kind: "header" | "param";
  namePlaceholder: string;
}) {
  const problem = overrideProblem(value, kind);
  const set = (i: number, patch: Partial<KVOverride>) =>
    onChange(value.map((ov, j) => (j === i ? { ...ov, ...patch } : ov)));
  return (
    <SettingsRow
      stacked
      title={title}
      description={
        <>
          {help}
          {problem ? (
            <span className="mt-1 block text-xs text-danger" role="alert">
              {problem}
            </span>
          ) : null}
        </>
      }
    >
      <div className="space-y-2">
        {value.map((ov, i) => (
          <div key={i} className="flex flex-wrap items-center gap-2">
            <Input
              className="w-40"
              aria-label={t("Name")}
              placeholder={namePlaceholder}
              value={ov.name}
              onChange={(e) => set(i, { name: e.currentTarget.value })}
            />
            <Input
              className="min-w-40 flex-1"
              aria-label={t("Value")}
              placeholder={t("Value")}
              value={ov.value ?? ""}
              disabled={Boolean(ov.remove)}
              onChange={(e) => set(i, { value: e.currentTarget.value })}
            />
            <label className="inline-flex items-center gap-1 text-xs text-fg-muted">
              <input
                type="checkbox"
                checked={Boolean(ov.remove)}
                onChange={(e) => set(i, { remove: e.currentTarget.checked || undefined })}
              />
              {t("Remove")}
            </label>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              aria-label={t("Delete row")}
              onClick={() => onChange(value.filter((_, j) => j !== i))}
            >
              <Trash2 size={14} />
            </Button>
          </div>
        ))}
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() => onChange([...value, { name: "", value: "" }])}
        >
          <Plus size={14} /> {t("Add row")}
        </Button>
      </div>
    </SettingsRow>
  );
}

/**
 * "Request identity" inside the indexer drawer: a plain-language profile
 * picker, a live masked preview of the exact request, advanced header and
 * parameter overrides, and a test that runs with this identity.
 */
export function RequestIdentitySection({
  draft,
  error,
  onChange,
}: {
  draft: Indexer;
  error?: string;
  onChange: (patch: Partial<Indexer>) => void;
}) {
  const ri = useMemo(() => effectiveIdentity(draft), [draft]);
  const [preview, setPreview] = useState<RequestPreview | null>(null);
  const [previewError, setPreviewError] = useState<string>("");
  const [test, setTest] = useState<TestState>({ status: "idle" });

  const patch = (part: Partial<IndexerRequestIdentity>) => {
    const next = { ...ri, ...part };
    onChange({
      requestIdentity: next,
      userAgent: mirroredUserAgent(next, preview?.profiles),
    });
  };

  const previewKey = JSON.stringify([
    draft.url,
    draft.apiKey,
    draft.requestIdentity,
    draft.userAgent,
  ]);
  useEffect(() => {
    let live = true;
    const timer = setTimeout(() => {
      api
        .previewIndexer({ ...draft, requestIdentity: effectiveIdentity(draft) })
        .then((p) => {
          if (live) {
            setPreview(p);
            setPreviewError("");
          }
        })
        .catch((e: unknown) => {
          if (live) setPreviewError(errorMessage(e));
        });
    }, 350);
    return () => {
      live = false;
      clearTimeout(timer);
    };
    // previewKey captures every draft field the preview depends on.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [previewKey]);

  async function runIdentityTest() {
    setTest({ status: "running" });
    try {
      const r = await api.testIndexer({ ...draft, requestIdentity: ri });
      setTest(
        r.ok
          ? {
              status: "ok",
              durationMs: r.durationMs,
              message: t("The indexer answered a capabilities request with this identity."),
            }
          : { status: "fail", message: r.error ?? t("failed") },
      );
    } catch (e) {
      setTest({ status: "fail", message: errorMessage(e) });
    }
  }

  const options = [{ value: "", label: profileLabel("") }].concat(
    PROFILE_IDS.map((id) => ({ value: id, label: profileLabel(id) })),
  );
  const grabPaired = preview?.profiles.find((p) => p.id === ri.profile)?.grabPaired;
  const grabOptions = [
    {
      value: "",
      label: grabPaired
        ? t("Automatic ({name})", { name: profileLabel(grabPaired).replace(/^Look like /, "") })
        : t("Same as searches"),
    },
  ].concat(PROFILE_IDS.map((id) => ({ value: id, label: profileLabel(id) })));

  const renderCall = (call: RequestPreview["search"]) =>
    [call.requestLine, ...call.headers.map((h) => `${h.name}: ${h.value}`)].join("\n");

  return (
    <SettingsSection
      title={t("Request identity")}
      description={t(
        "How this indexer sees Ether's requests. Some indexers prefer a familiar client.",
      )}
    >
      <SettingsRow
        title={t("Searches and capability checks")}
        description={
          <>
            {t(
              "Pick the client this indexer should see. Indexers may block or ban clients that pretend to be another app.",
            )}
            {error ? (
              <span className="mt-1 block text-xs text-danger" role="alert">
                {error}
              </span>
            ) : null}
          </>
        }
        control={
          <div className="w-64">
            <SearchSelect
              {...searchSelectLabels()}
              ariaLabel={t("Searches and capability checks")}
              value={ri.profile ?? ""}
              options={options}
              onChange={(v) => patch({ profile: v })}
            />
          </div>
        }
      />
      <SettingsRow
        title={t("NZB and torrent downloads")}
        description={t("Downloads usually come from a downloader app, so they can look like one.")}
        control={
          <div className="w-64">
            <SearchSelect
              {...searchSelectLabels()}
              ariaLabel={t("NZB and torrent downloads")}
              value={ri.grabProfile ?? ""}
              options={grabOptions}
              onChange={(v) => patch({ grabProfile: v || undefined })}
            />
          </div>
        }
      />
      <SettingsRow
        stacked
        title={t("What the indexer sees")}
        description={t("A sample search with this identity. Your API key is hidden.")}
      >
        {preview ? (
          <div className="space-y-2">
            <pre className="overflow-x-auto rounded-md bg-surface-2 p-3 text-xs leading-relaxed">
              {renderCall(preview.search)}
            </pre>
            <p className="text-xs font-medium text-fg-muted">{t("And for a download:")}</p>
            <pre className="overflow-x-auto rounded-md bg-surface-2 p-3 text-xs leading-relaxed">
              {renderCall(preview.grab)}
            </pre>
          </div>
        ) : (
          <p className="text-sm text-fg-muted" role="status">
            {previewError
              ? t("Preview unavailable: {message}", { message: previewError })
              : t("Loading preview…")}
          </p>
        )}
      </SettingsRow>
      <SettingsRow
        title={t("Try it")}
        description={t("Ask the indexer for its capabilities using this identity.")}
        control={
          <TestButton
            label={t("Test this identity")}
            state={test}
            onTest={() => void runIdentityTest()}
            disabled={!draft.url}
          />
        }
      />
      <MoreOptions label={t("Advanced: exact headers and parameters")}>
        <OverrideRows
          title={t("Search headers")}
          help={t(
            "Add, change or remove a header on searches and capability checks, on top of the profile above. Login headers like Authorization are hidden after saving.",
          )}
          kind="header"
          namePlaceholder={t("Header name")}
          value={ri.headers ?? []}
          onChange={(v) => patch({ headers: v.length ? v : undefined })}
        />
        <OverrideRows
          title={t("Search parameters")}
          help={t(
            "Add, change or remove a query parameter on searches. Applied last; apikey and t cannot be removed.",
          )}
          kind="param"
          namePlaceholder={t("Parameter name")}
          value={ri.params ?? []}
          onChange={(v) => patch({ params: v.length ? v : undefined })}
        />
        <OverrideRows
          title={t("Download headers")}
          help={t("Add, change or remove a header on NZB and torrent downloads.")}
          kind="header"
          namePlaceholder={t("Header name")}
          value={ri.grabHeaders ?? []}
          onChange={(v) => patch({ grabHeaders: v.length ? v : undefined })}
        />
        <OverrideRows
          title={t("Download parameters")}
          help={t(
            "Add, change or remove a query parameter when Ether builds the download address itself. Addresses from the indexer are left untouched.",
          )}
          kind="param"
          namePlaceholder={t("Parameter name")}
          value={ri.grabParams ?? []}
          onChange={(v) => patch({ grabParams: v.length ? v : undefined })}
        />
      </MoreOptions>
    </SettingsSection>
  );
}
