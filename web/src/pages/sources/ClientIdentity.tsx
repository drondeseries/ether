import { SettingsSection, Spinner, toast } from "@braedonsaunders/appkit-ui";
import { useMemo, useState } from "react";
import { MoreOptions } from "../../components/ConfigSection";
import { ErrorPanel } from "../../components/feedback";
import { MapInput } from "../../components/fields";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { t, tn } from "../../i18n";
import { api, ApiError } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import type { ClientIdentityConfig, Indexer } from "../../lib/types";
import { TextRow } from "./form";

/**
 * The owner's client-identity policy: the user agent Newznab and Torznab
 * indexers see, custom origin-confined headers, and how many indexers inherit
 * the policy.
 */
export function ClientIdentity({
  indexers,
  onSaved,
}: {
  /** The page's live indexer list, so the inherit count follows edits. */
  indexers: Indexer[];
  onSaved?: () => void;
}) {
  const section = useAsync(() => api.getSection<ClientIdentityConfig | null>("clientIdentity"), []);
  const saved = useMemo(() => section.data?.value ?? {}, [section.data]);
  const { draft, setDraft, dirty, reset } = useDraft<ClientIdentityConfig>(saved);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [errors, setErrors] = useState<Record<string, string>>({});

  if (section.error) {
    return <ErrorPanel error={section.error} onRetry={() => void section.reload()} />;
  }
  if (!section.data || !draft) {
    return (
      <div className="py-10 text-center">
        <Spinner size={20} label={t("Loading settings")} />
      </div>
    );
  }

  const policy = draft;
  const defaultUserAgent = section.data.defaultUserAgent ?? "";

  // Only the Newznab/Torznab client resolves this policy; built-in sites and
  // Stremio addons have their own request paths.
  const capable = indexers.filter((ix) => !ix.kind || ix.kind === "newznab");
  const inherited = capable.filter(
    (ix) => !ix.userAgent?.trim() && !ix.requestIdentity?.profile,
  ).length;
  const overrides = capable.length - inherited;
  const backend = indexers.length - capable.length;

  const patch = (part: Partial<ClientIdentityConfig>) => setDraft({ ...policy, ...part });
  const save = async () => {
    setSaving(true);
    setError(null);
    setErrors({});
    try {
      await api.putSection("clientIdentity", {
        userAgent: policy.userAgent ?? "",
        headers: policy.headers ?? {},
      });
      toast.success(t("Settings saved"), { description: t("Client identity") });
      await section.reload();
      onSaved?.();
    } catch (e) {
      setError(e);
      if (e instanceof ApiError && e.fields) {
        setErrors(
          Object.fromEntries(
            e.fields.map((f) => [f.field.replace(/^clientIdentity\./, ""), f.message]),
          ),
        );
      }
    } finally {
      setSaving(false);
    }
  };

  return (
    <>
      <SettingsSection
        title={t("Client identity")}
        description={t("What Newznab and Torznab indexers see when Ether searches or downloads.")}
      >
        <TextRow
          label={t("User agent")}
          value={String(policy.userAgent ?? "")}
          onChange={(v) => patch({ userAgent: v })}
          placeholder={defaultUserAgent}
          help={t(
            "Leave empty to identify as Ether. Indexers may block or ban clients that pretend to be another app.",
          )}
          error={errors.userAgent}
        />

        {capable.length > 0 ? (
          <div className="px-5 py-3">
            <p className="text-sm text-fg-muted">
              {overrides === 0
                ? tn(
                    capable.length,
                    "{count} indexer inherits this policy",
                    "All {count} indexers inherit this policy",
                  )
                : tn(
                    overrides,
                    "{inherited} of {total} indexers inherit this policy; {count} has its own identity",
                    "{inherited} of {total} indexers inherit this policy; {count} have their own identity",
                    { inherited, total: capable.length },
                  )}
            </p>
            {backend > 0 ? (
              <p className="mt-0.5 text-xs text-fg-subtle">
                {t("Built-in sites and Stremio addons use their own identity.")}
              </p>
            ) : null}
          </div>
        ) : null}

        <MoreOptions label={t("More options: {title}", { title: t("Client identity") })}>
          <SettingsRow
            stacked
            title={t("Custom headers")}
            description={
              <>
                {t(
                  "Sent to every Newznab and Torznab indexer on its own origin, so don't put one site's login here. Values are sealed at rest and shown as •••• after saving.",
                )}
                {errors.headers ? (
                  <span className="mt-1 block text-xs text-danger" role="alert">
                    {errors.headers}
                  </span>
                ) : null}
              </>
            }
          >
            <MapInput
              ariaLabel={t("Custom headers")}
              value={(policy.headers as Record<string, string> | null) ?? {}}
              onChange={(v) => patch({ headers: v })}
              namePlaceholder={t("Header name")}
              valuePlaceholder={t("Value")}
              invalid={Boolean(errors.headers)}
            />
          </SettingsRow>
        </MoreOptions>
      </SettingsSection>
      <SaveBar
        dirty={dirty}
        saving={saving}
        error={error}
        onSave={save}
        onDiscard={() => {
          reset();
          setErrors({});
          setError(null);
        }}
      />
    </>
  );
}
