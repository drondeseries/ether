import { Badge, Button, EmptyState, Input, Select, Switch, cn, confirmDialog, toast } from "@braedonsaunders/appkit-ui";
import {
  Activity,
  Bell,
  FlaskConical,
  Gauge,
  Lightbulb,
  Newspaper,
  OctagonX,
  Pencil,
  PinOff,
  Play,
  Plus,
  Search,
  Siren,
  Smartphone,
  Sparkles,
  Timer,
  Trash2,
  TriangleAlert,
  Workflow,
  type LucideIcon,
} from "lucide-react";
import { useMemo, useState } from "react";
import { useLocation, useNavigate, useParams } from "react-router-dom";
import { Drawer } from "../../components/Drawer";
import { Field } from "../../components/fields";
import { ErrorPanel, LoadingRows, MutationError } from "../../components/feedback";
import { Kpi } from "../../components/Kpi";
import { DomainPage, Section } from "../../components/layout";
import { localized, t, tn } from "../../i18n";
import { apiFetch, errorMessage } from "../../lib/api";
import { flowsApi } from "../../lib/flowsApi";
import { relativeTime } from "../../lib/format";
import { useAsync, useInterval } from "../../lib/hooks";
import { useStreamEvent } from "../../lib/stream";
import type { NotificationsData } from "../notifications/types";
import { DescribeFlowDialog } from "./DescribeFlowDialog";
import { FlowEditor } from "./Editor";
import { StepTrace } from "./RunTrace";
import { RunStatusBadge, useCatalogue } from "./shared";
import { TestRunDrawer } from "./TestRunDrawer";
import type { Flow, FlowTemplate, Run, RunStatus } from "./types";

const TABS = localized(
  () =>
    [
      { key: "flows", label: t("My flows") },
      { key: "templates", label: t("Templates") },
      { key: "runs", label: t("Runs") },
    ] as const,
);

const TEMPLATE_ICONS: Record<string, LucideIcon> = {
  OctagonX, Timer, Gauge, Bell, Newspaper, Lightbulb, Smartphone, PinOff, Siren,
};

/** Flows home (list / templates / runs) plus the full-screen canvas editor at /flows/edit/:id. */
export default function Flows() {
  const location = useLocation();
  const params = useParams();
  const splat = (params["*"] ?? location.pathname.replace(/^\/flows\/?/, "")).split("/").filter(Boolean);

  if (splat[0] === "edit") return <FlowEditor key={`edit:${splat[1] ?? ""}`} flowId={splat[1]} />;
  if (splat[0] === "new") return <FlowEditor key={`new:${location.key}`} />;

  const tab = splat[0] === "templates" ? "templates" : splat[0] === "runs" ? "runs" : "flows";
  return (
    <DomainPage
      title={t("Flows")}
      description={t("Automations for your whole server — when something happens, check a few things, then act. Flows react off to the side; they never sit in the way of playback.")}
      base="/flows"
      tabs={TABS}
      active={tab}
      tabHref={(key) => (key === "flows" ? "/flows" : `/flows/${key}`)}
    >
      {tab === "templates" ? <TemplatesTab /> : tab === "runs" ? <RunsTab /> : <FlowListTab />}
    </DomainPage>
  );
}

function useFlowsList() {
  const state = useAsync(() => flowsApi.listFlows(), []);
  useStreamEvent("flows.changed", () => void state.reload());
  useStreamEvent("flows.run", () => void state.reload());
  useInterval(() => void state.reload(), 30_000);
  return state;
}

function FlowListTab() {
  const navigate = useNavigate();
  const flowsState = useFlowsList();
  const catalogueState = useCatalogue();
  const [query, setQuery] = useState("");
  const [describeOpen, setDescribeOpen] = useState(false);
  const [testFlow, setTestFlow] = useState<Flow | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);

  const flows = flowsState.data ?? [];
  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    return q ? flows.filter((f) => f.name.toLowerCase().includes(q) || (f.description ?? "").toLowerCase().includes(q)) : flows;
  }, [flows, query]);

  const stats = useMemo(
    () => ({
      enabled: flows.filter((f) => f.enabled).length,
      runs24h: flows.reduce((s, f) => s + (f.runs24h ?? 0), 0),
      failures24h: flows.reduce((s, f) => s + (f.failures24h ?? 0), 0),
      active: flows.reduce((s, f) => s + (f.activeRuns ?? 0), 0),
    }),
    [flows],
  );

  const act = async (flow: Flow, fn: () => Promise<void>, ok?: string) => {
    setBusyId(flow.id);
    try {
      await fn();
      if (ok) toast.success(ok);
      await flowsState.reload();
    } catch (e) {
      toast.error(errorMessage(e));
      await flowsState.reload();
    } finally {
      setBusyId(null);
    }
  };

  return (
    <div className="space-y-6">
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Kpi label={t("Automations on")} value={stats.enabled} detail={tn(flows.length, "{count} flow in total", "{count} flows in total")} icon={<Workflow size={17} />} tone="primary" />
        <Kpi label={t("Runs in 24 h")} value={stats.runs24h} detail={stats.active ? tn(stats.active, "{count} run in progress now", "{count} runs in progress now") : t("Nothing running right now")} icon={<Activity size={17} />} tone="info" />
        <Kpi label={t("Failures in 24 h")} value={stats.failures24h} detail={stats.failures24h ? t("Open Runs to see what went wrong") : t("Everything ran cleanly")} icon={<TriangleAlert size={17} />} tone={stats.failures24h ? "danger" : "success"} />
        <Kpi label={t("Watching for")} value={t("Events, schedules & hooks")} detail={t("119 event types across the server")} icon={<Sparkles size={17} />} tone="success" />
      </div>

      <Section
        title={t("My flows")}
        description={t("Each flow listens, checks, and acts — edit one on the canvas to see how.")}
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => setDescribeOpen(true)}>
              <Sparkles size={14} /> {t("Describe it")}
            </Button>
            <Button size="sm" onClick={() => navigate("/flows/new")}>
              <Plus size={14} /> {t("New flow")}
            </Button>
          </>
        }
        bodyClassName={visible.length ? "divide-y divide-border" : undefined}
      >
        <div className="px-5 pt-4">
          <label className="flex max-w-sm items-center gap-2 rounded-lg border border-border px-2.5 py-2">
            <Search size={14} className="shrink-0 text-fg-subtle" />
            <input value={query} onChange={(e) => setQuery(e.target.value)} placeholder={t("Search flows…")} aria-label={t("Search flows")} className="min-w-0 flex-1 bg-transparent text-sm text-fg outline-none placeholder:text-fg-subtle" />
          </label>
        </div>
        {flowsState.error && !flowsState.data ? (
          <div className="px-5 py-4"><ErrorPanel error={flowsState.error} onRetry={() => void flowsState.reload()} /></div>
        ) : !flowsState.data ? (
          <div className="px-5 py-4"><LoadingRows rows={4} /></div>
        ) : visible.length === 0 ? (
          <EmptyState
            icon={<Workflow />}
            title={flows.length ? t("No flows match") : t("No flows yet")}
            description={flows.length ? t("Try a different search.") : t("Describe what should happen, or start from a template — the first flow takes a minute.")}
            action={
              flows.length ? undefined : (
                <div className="flex gap-2">
                  <Button size="sm" onClick={() => setDescribeOpen(true)}><Sparkles size={14} /> {t("Describe it")}</Button>
                  <Button size="sm" variant="outline" onClick={() => navigate("/flows/templates")}>{t("Browse templates")}</Button>
                </div>
              )
            }
          />
        ) : (
          visible.map((flow) => (
            <div key={flow.id} className="flex flex-wrap items-center gap-3 px-5 py-4">
              <span aria-hidden className={cn("size-2.5 shrink-0 rounded-full", !flow.enabled ? "bg-fg-subtle/40" : flow.lastStatus === "failed" ? "bg-danger" : flow.lastStatus === "waiting" || flow.lastStatus === "running" ? "animate-pulse bg-primary" : "bg-success")} />
              <div className="min-w-0 flex-1 basis-56">
                <div className="flex flex-wrap items-center gap-2">
                  <button type="button" onClick={() => navigate(`/flows/edit/${flow.id}`)} className="truncate text-left font-medium text-fg hover:underline">
                    {flow.name}
                  </button>
                  {flow.lastStatus ? <RunStatusBadge status={flow.lastStatus} /> : null}
                  {flow.disabledReason ? <Badge variant="destructive">{t("Turned itself off")}</Badge> : null}
                </div>
                <p className="mt-0.5 truncate text-sm text-fg-muted" title={flow.description}>{flow.description || flow.triggerSummary}</p>
                <p className="mt-1 flex flex-wrap gap-x-3 text-xs text-fg-subtle">
                  <span>{flow.triggerSummary ?? ""}</span>
                  <span>{flow.lastRunAt ? t("last run {when}", { when: relativeTime(flow.lastRunAt) }) : t("never run yet")}</span>
                  <span>{tn(flow.runs24h ?? 0, "{count} run in 24 h", "{count} runs in 24 h")}</span>
                  {(flow.failures24h ?? 0) > 0 ? <span className="font-medium text-danger">{tn(flow.failures24h ?? 0, "{count} failure", "{count} failures")}</span> : null}
                  {(flow.activeRuns ?? 0) > 0 ? <span className="font-medium text-primary">{t("{count} in progress", { count: flow.activeRuns ?? 0 })}</span> : null}
                </p>
                {flow.disabledReason ? <p className="mt-1 text-xs text-danger">{flow.disabledReason}</p> : null}
              </div>
              <div className="flex shrink-0 flex-wrap items-center gap-2">
                <label className="flex items-center gap-1.5 text-xs text-fg-muted">
                  <Switch
                    aria-label={t("Enable {name}", { name: flow.name })}
                    checked={flow.enabled}
                    disabled={busyId === flow.id}
                    onChange={(e) => void act(flow, async () => { if (e.target.checked) await flowsApi.enableFlow(flow.id); else await flowsApi.disableFlow(flow.id); })}
                  />
                  {flow.enabled ? t("On") : t("Off")}
                </label>
                <Button variant="outline" size="sm" disabled={!catalogueState.data} onClick={() => setTestFlow(flow)}>
                  <FlaskConical size={13} /> {t("Test")}
                </Button>
                {flow.graph.nodes.some((n) => n.kind === "trigger.manual") ? (
                  <Button variant="outline" size="sm" disabled={busyId === flow.id} onClick={() => void act(flow, async () => { await flowsApi.runManual(flow.id); }, t("Flow started"))}>
                    <Play size={13} /> {t("Run now")}
                  </Button>
                ) : null}
                <Button variant="outline" size="sm" onClick={() => navigate(`/flows/edit/${flow.id}`)}>
                  <Pencil size={13} /> {t("Edit")}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  aria-label={t("Delete {name}", { name: flow.name })}
                  disabled={busyId === flow.id}
                  onClick={async () => {
                    const ok = await confirmDialog({
                      title: t("Delete this flow?"),
                      message: t("“{name}” and its run history will be removed. Active runs are cancelled.", { name: flow.name }),
                      confirmLabel: t("Delete"),
                      tone: "danger",
                    });
                    if (ok) void act(flow, async () => { await flowsApi.deleteFlow(flow.id); }, t("Flow deleted"));
                  }}
                >
                  <Trash2 size={14} />
                </Button>
              </div>
            </div>
          ))
        )}
      </Section>

      <DescribeFlowDialog
        open={describeOpen}
        onClose={() => setDescribeOpen(false)}
        onDraft={(draft) => navigate("/flows/new", { state: { draft } })}
      />
      {testFlow && catalogueState.data ? (
        <TestRunDrawer open onClose={() => setTestFlow(null)} flowId={testFlow.id} graph={testFlow.graph} catalogue={catalogueState.data} />
      ) : null}
    </div>
  );
}

function TemplatesTab() {
  const navigate = useNavigate();
  const templatesState = useAsync(() => flowsApi.listTemplates(), []);
  const [category, setCategory] = useState("all");
  const [using, setUsing] = useState<FlowTemplate | null>(null);
  const templates = templatesState.data ?? [];
  const categories = useMemo(() => [...new Set(templates.map((x) => x.category))], [templates]);
  const visible = category === "all" ? templates : templates.filter((x) => x.category === category);

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap gap-1.5" role="group" aria-label={t("Template categories")}>
        {["all", ...categories].map((c) => (
          <button
            key={c}
            type="button"
            aria-pressed={category === c}
            onClick={() => setCategory(c)}
            className={cn(
              "rounded-full border px-3 py-1 text-sm transition-colors",
              category === c ? "border-primary bg-primary-subtle text-primary" : "border-border text-fg-muted hover:text-fg",
            )}
          >
            {c === "all" ? t("Everything") : c}
          </button>
        ))}
      </div>
      {templatesState.error && !templatesState.data ? (
        <ErrorPanel error={templatesState.error} onRetry={() => void templatesState.reload()} />
      ) : !templatesState.data ? (
        <LoadingRows rows={4} />
      ) : (
        <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
          {visible.map((tpl) => {
            const Icon = (tpl.icon && TEMPLATE_ICONS[tpl.icon]) || Workflow;
            return (
              <article key={tpl.id} className="flex flex-col rounded-xl border border-border bg-surface p-4 shadow-xs transition-shadow hover:shadow">
                <div className="flex items-start gap-3">
                  <span className="grid size-10 shrink-0 place-items-center rounded-lg bg-primary-subtle text-primary">
                    <Icon size={18} />
                  </span>
                  <div className="min-w-0">
                    <h3 className="font-medium text-fg">{tpl.name}</h3>
                    <p className="text-xs text-fg-subtle">{tpl.category} · {tn(tpl.graph.nodes.length, "{count} step", "{count} steps")}</p>
                  </div>
                </div>
                <p className="mt-2.5 flex-1 text-sm leading-relaxed text-fg-muted">{tpl.description}</p>
                <div className="mt-3 flex items-center gap-2">
                  <Button size="sm" onClick={() => setUsing(tpl)}>{t("Use this template")}</Button>
                  <Button size="sm" variant="ghost" onClick={() => navigate("/flows/new", { state: { draft: { name: tpl.name, description: tpl.description, graph: tpl.graph } } })}>
                    {t("Preview on canvas")}
                  </Button>
                </div>
              </article>
            );
          })}
        </div>
      )}
      {using ? <UseTemplateDrawer template={using} onClose={() => setUsing(null)} /> : null}
    </div>
  );
}

function UseTemplateDrawer({ template, onClose }: { template: FlowTemplate; onClose: () => void }) {
  const navigate = useNavigate();
  const [name, setName] = useState(template.name);
  const [enabled, setEnabled] = useState(true);
  const [channels, setChannels] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const notices = useAsync(() => apiFetch<NotificationsData>("/notifications/").catch(() => null), []);
  const wantsChannels = (template.params ?? []).some((p) => p.picker === "notifyChannels");

  const create = async () => {
    setBusy(true);
    setError(null);
    try {
      const flow = await flowsApi.createFromTemplate(template.id, {
        name: name.trim() || template.name,
        enabled,
        params: wantsChannels ? { channelIds: channels } : undefined,
      });
      toast.success(t("Flow created from template"));
      onClose();
      navigate(`/flows/edit/${flow.id}`);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Drawer
      open
      onClose={onClose}
      title={template.name}
      footer={
        <div className="flex flex-wrap justify-end gap-2">
          <Button variant="ghost" onClick={onClose}>{t("Cancel")}</Button>
          <Button disabled={busy} onClick={() => void create()}>{busy ? t("Creating…") : t("Create & open in editor")}</Button>
        </div>
      }
    >
      <div className="space-y-4">
        <p className="text-sm text-fg-muted">{template.description}</p>
        <Field label={t("Name")}>
          <Input aria-label={t("Flow name")} value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        {wantsChannels ? (
          <Field label={t("Send to")} hint={t("Leave empty to use all enabled channels. You can change this per step later.")}>
            <div className="flex flex-wrap gap-1">
              {(notices.data?.channels ?? []).map((c) => (
                <button
                  key={c.id}
                  type="button"
                  aria-pressed={channels.includes(c.id)}
                  onClick={() => setChannels((prev) => (prev.includes(c.id) ? prev.filter((x) => x !== c.id) : [...prev, c.id]))}
                  className={cn(
                    "rounded-full border px-2.5 py-1 text-xs transition-colors",
                    channels.includes(c.id) ? "border-primary bg-primary-subtle text-primary" : "border-border text-fg-muted hover:text-fg",
                  )}
                >
                  {c.name || c.kind}
                </button>
              ))}
              {(notices.data?.channels ?? []).length === 0 ? <span className="text-sm text-fg-muted">{t("No notification channels yet — the flow will use all channels once you add one.")}</span> : null}
            </div>
          </Field>
        ) : null}
        <label className="flex items-center justify-between gap-3">
          <span className="text-sm text-fg">{t("Turn on right away")}</span>
          <Switch aria-label={t("Enabled")} checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
        </label>
        <MutationError error={error} />
      </div>
    </Drawer>
  );
}

const RUN_FILTERS: { key: RunStatus | ""; label: () => string }[] = [
  { key: "", label: () => t("All runs") },
  { key: "running", label: () => t("Running") },
  { key: "waiting", label: () => t("Waiting") },
  { key: "completed", label: () => t("Completed") },
  { key: "failed", label: () => t("Failed") },
  { key: "cancelled", label: () => t("Cancelled") },
];

function RunsTab() {
  const [status, setStatus] = useState<RunStatus | "">("");
  const [flowId, setFlowId] = useState("");
  const [openRun, setOpenRun] = useState<string | null>(null);
  const flowsState = useAsync(() => flowsApi.listFlows(), []);
  const runsState = useAsync(() => flowsApi.listRuns({ status, flowId: flowId || undefined, limit: 60 }), [status, flowId]);
  useStreamEvent("flows.run", () => void runsState.reload());
  useInterval(() => void runsState.reload(), 15_000);

  const runs = runsState.data?.runs ?? [];
  const eventLabel = useEventLabels();

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex flex-wrap gap-1.5" role="group" aria-label={t("Filter by status")}>
          {RUN_FILTERS.map((f) => (
            <button
              key={f.key || "all"}
              type="button"
              aria-pressed={status === f.key}
              onClick={() => setStatus(f.key)}
              className={cn(
                "rounded-full border px-3 py-1 text-sm transition-colors",
                status === f.key ? "border-primary bg-primary-subtle text-primary" : "border-border text-fg-muted hover:text-fg",
              )}
            >
              {f.label()}
            </button>
          ))}
        </div>
        <Select aria-label={t("Filter by flow")} value={flowId} onChange={(e) => setFlowId(e.target.value)} className="ml-auto w-56 max-w-full">
          <option value="">{t("Every flow")}</option>
          {(flowsState.data ?? []).map((f) => (
            <option key={f.id} value={f.id}>{f.name}</option>
          ))}
        </Select>
      </div>

      <Section bodyClassName={runs.length ? "divide-y divide-border" : undefined}>
        {runsState.error && !runsState.data ? (
          <div className="px-5 py-4"><ErrorPanel error={runsState.error} onRetry={() => void runsState.reload()} /></div>
        ) : !runsState.data ? (
          <div className="px-5 py-4"><LoadingRows rows={5} /></div>
        ) : runs.length === 0 ? (
          <EmptyState icon={<Activity />} title={t("No runs yet")} description={t("Runs appear here the moment a flow fires — test one from the editor to see a trace right away.")} />
        ) : (
          runs.map((run) => (
            <button key={run.id} type="button" onClick={() => setOpenRun(run.id)} className="flex w-full flex-wrap items-center gap-3 px-5 py-3.5 text-left transition-colors hover:bg-surface-hover">
              <RunStatusBadge status={run.status} />
              <span className="min-w-0 flex-1 basis-48">
                <span className="block truncate font-medium text-fg">{run.flowName ?? run.flowId}</span>
                <span className="block text-xs text-fg-muted">
                  {run.eventType ? eventLabel(run.eventType) : t("Manual run")}
                  {run.dryRun ? ` · ${t("test")}` : ""}
                  {run.error ? ` · ${run.error}` : ""}
                </span>
              </span>
              <span className="text-xs text-fg-subtle">{tn(run.stepCount, "{count} step", "{count} steps")}</span>
              <span className="text-sm tabular-nums text-fg-muted">{relativeTime(run.startedAt)}</span>
            </button>
          ))
        )}
      </Section>
      {openRun ? <RunDetailDrawer runId={openRun} onClose={() => setOpenRun(null)} onChanged={() => void runsState.reload()} /> : null}
    </div>
  );
}

function useEventLabels() {
  const catalogue = useCatalogue();
  return (type: string) => catalogue.data?.eventsByType.get(type)?.label ?? type;
}

function RunDetailDrawer({ runId, onClose, onChanged }: { runId: string; onClose: () => void; onChanged: () => void }) {
  const catalogueState = useCatalogue();
  const detail = useAsync(async () => {
    const d = await flowsApi.getRun(runId);
    const flow = await flowsApi.getFlow(d.run.flowId).catch(() => null);
    return { ...d, flow };
  }, [runId]);
  useStreamEvent("flows.run", () => void detail.reload());
  const run: Run | undefined = detail.data?.run;
  const canCancel = run && (run.status === "running" || run.status === "waiting" || run.status === "pending");

  return (
    <Drawer
      open
      onClose={onClose}
      title={run?.flowName ?? t("Run")}
      size="xl"
      footer={
        canCancel ? (
          <div className="flex justify-end">
            <Button
              variant="outline"
              onClick={async () => {
                try {
                  await flowsApi.cancelRun(runId);
                  toast.success(t("Run cancelled"));
                  onChanged();
                  void detail.reload();
                } catch (e) {
                  toast.error(errorMessage(e));
                }
              }}
            >
              {t("Cancel this run")}
            </Button>
          </div>
        ) : undefined
      }
    >
      {detail.error && !detail.data ? (
        <ErrorPanel error={detail.error} onRetry={() => void detail.reload()} />
      ) : !detail.data ? (
        <LoadingRows rows={4} />
      ) : (
        <StepTrace run={detail.data.run} steps={detail.data.steps} graph={detail.data.flow?.graph} catalogue={catalogueState.data} />
      )}
    </Drawer>
  );
}
