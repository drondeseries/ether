import { sourceName } from "../../libraries/CatalogueSources";
import { Button, SettingsSection, toast } from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { SettingsRow } from "../../../components/SettingsRow";
import { t, tn } from "../../../i18n";
import { errorMessage } from "../../../lib/api";
import { formatDateTime, formatNumber } from "../../../lib/format";
import { useAsync } from "../../../lib/hooks";
import { librariesApi } from "../../libraries/api";
export function PluginCatalogueSettings({
  pluginId,
  onSetup,
}: {
  pluginId: string;
  onSetup: () => void;
}) {
  const libraries = useAsync(
    (signal) => librariesApi.setup(pluginId, signal).then((setup) => setup.libraries),
    [pluginId],
  );
  const sources = useAsync((signal) => librariesApi.catalogueSources(signal), [pluginId]);
  const [busy, setBusy] = useState(false);
  const sync = async (id: number, full: boolean) => {
    setBusy(true);
    try {
      await librariesApi.sync(id, full);
      toast.success(t("Settings saved"));
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      void libraries.reload();
      void sources.reload();
      setBusy(false);
    }
  };
  const owned = libraries.data?.filter((lib) => lib.pluginId === pluginId) ?? [];
  const catalogues =
    sources.data?.filter((source) => source.pluginId === pluginId && source.group === "stremio") ??
    [];
  if (!owned.length && !catalogues.length) return null;
  return (
    <SettingsSection title={t("Keeping it up to date")}>
      {owned.map((lib) => {
        const used = lib.storedItems ?? lib.itemCount;
        const countAgainstCap =
          typeof used === "number" &&
          typeof lib.maxPluginItems === "number" &&
          lib.maxPluginItems > 0
            ? t("{used} of {limit} items", {
                used: formatNumber(used, 0),
                limit: formatNumber(lib.maxPluginItems, 0),
              })
            : "";
        const status =
          lib.sync?.lastError ??
          (lib.sync?.lastOk ? formatDateTime(lib.sync.lastOk) : t("Not synced yet"));
        return (
          <SettingsRow
            key={lib.id}
            title={lib.kind?.label ?? lib.name}
            description={countAgainstCap ? `${countAgainstCap} · ${status}` : status}
            control={
              <div className="flex flex-wrap gap-2">
                <Button
                  size="sm"
                  variant="outline"
                  disabled={busy}
                  onClick={() => void sync(lib.id, true)}
                >
                  {t("Full re-sync")}
                </Button>
                <Button size="sm" disabled={busy} onClick={() => void sync(lib.id, false)}>
                  {t("Sync now")}
                </Button>
              </div>
            }
          />
        );
      })}
      {catalogues.map((source) => {
        const countLabel =
          typeof source.itemCount === "number"
            ? tn(source.itemCount, "{count} title", "{count} titles")
            : "";
        const statusLabel = source.lastError
          ? source.lastError
          : source.health === "unknown"
            ? t("Not loaded yet")
            : source.health === "ok"
              ? t("Healthy")
              : t("Needs attention");
        return (
          <SettingsRow
            key={source.id}
            title={sourceName(source)}
            description={countLabel ? `${countLabel} · ${statusLabel}` : statusLabel}
            control={
              <Button
                size="sm"
                disabled={busy}
                onClick={async () => {
                  setBusy(true);
                  try {
                    await librariesApi.syncCatalogueSource(source.id);
                    toast.success(t("Settings saved"));
                  } catch (e) {
                    toast.error(errorMessage(e));
                  } finally {
                    void sources.reload();
                    setBusy(false);
                  }
                }}
              >
                {t("Sync now")}
              </Button>
            }
          />
        );
      })}
      {owned.length ? (
        <SettingsRow
          title={t("Setup")}
          description={t("Re-run the guided setup or check a step any time.")}
          control={
            <Button size="sm" variant="outline" onClick={onSetup}>
              {t("Open setup")}
            </Button>
          }
        />
      ) : null}
    </SettingsSection>
  );
}
