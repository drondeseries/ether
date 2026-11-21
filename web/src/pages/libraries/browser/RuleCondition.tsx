import { Button, Input, Select } from "@braedonsaunders/appkit-ui";
import { catalogValueLabel } from "./catalogValueLabel";
import { t } from "../../../i18n";
import type { MediaKind } from "../api";
import type { Rule, RuleField } from "../../../lib/contract";

const VIEWER_BROWSE_FIELDS = (): [string, string][] => [
  ["__history", t("Watch history")],
  ["__watched", t("Watched")],
  ["__unwatched", t("Unwatched")],
  ["__progress", t("In progress")],
  ["__favorite", t("Favorite")],
  ["__watchlist", t("On watchlist")],
  ["__lastViewedAfter", t("Watched after")],
  ["__lastViewedBefore", t("Watched before")],
];

// The extended rules use the catalogue's existing indexed predicates. These
// are individual rule conditions, never a second saved library filter.
export function browseFields(kind?: MediaKind): [string, string][] {
  if (kind)
    return [
      ["__group", t("Rules")],
      ...VIEWER_BROWSE_FIELDS(),
      ...["year", "rating", "released"].map((key): [string, string] => [
        key,
        kind.fields?.find((f) => f.key === key)?.label ??
          (key === "year" ? t("Year") : key === "rating" ? t("Rating") : t("Recently released")),
      ]),
      ...(kind.fields ?? [])
        .filter((f) => f.indexed && !["year", "rating", "released"].includes(f.key))
        .map((f): [string, string] => [f.key, f.label]),
    ];
  return [
    ["__group", t("Rules")],
    ...VIEWER_BROWSE_FIELDS(),
    ["__rated", t("Rated")],
    ["type", t("Type")],
    ["releaseGate", t("New-release gate")],
    ["playable", t("Playable now")],
    ["debridCached", t("Cached on debrid")],
    ["qualities", t("Quality")],
    ["anime", t("Anime keyword")],
    ["kids", t("Kids genre")],
    ["tags", t("Tags")],
    ["keywords", t("Keyword")],
    ["yearFrom", t("Minimum") + " · " + t("Year")],
    ["yearTo", t("Maximum") + " · " + t("Year")],
    ["years", t("Year")],
    ["excludeYears", t("Exclude") + " · " + t("Year")],
    ["minVotes", t("Minimum votes")],
    ["maxVotes", t("Maximum") + " · " + t("Votes")],
    ["minPopularity", t("Minimum popularity")],
    ["maxPopularity", t("Maximum") + " · " + t("Popularity")],
    ["dateAddedFrom", t("Minimum") + " · " + t("Release / air date")],
    ["dateAddedTo", t("Maximum") + " · " + t("Release / air date")],
    ["etherAddedFrom", t("Minimum") + " · " + t("Added to Ether")],
    ["etherAddedTo", t("Maximum") + " · " + t("Added to Ether")],
    ["statuses", t("Show status")],
    ["minSeasons", t("Minimum") + " · " + t("Seasons")],
    ["maxSeasons", t("Maximum") + " · " + t("Seasons")],
    ["minEpisodes", t("Minimum") + " · " + t("Episodes")],
    ["maxEpisodes", t("Maximum") + " · " + t("Episodes")],
    ["minRating", t("Minimum") + " · " + t("Rating")],
    ["maxRating", t("Maximum") + " · " + t("Rating")],
    ["excludeRatings", t("Exclude") + " · " + t("Rating")],
    ["maxRuntime", t("Maximum") + " · " + t("Runtime (minutes)")],
    ["minRuntime", t("Minimum") + " · " + t("Runtime (minutes)")],
    ["excludeRuntimes", t("Exclude") + " · " + t("Runtime (minutes)")],
    ["languages", t("Original language")],
    ["countries", t("Country")],
    ["excludeCountries", t("Exclude") + " · " + t("Country")],
    ["contentRatings", t("Certification")],
    ["excludeContentRatings", t("Exclude") + " · " + t("Certification")],
    ["maxContentRating", t("Maximum") + " · " + t("Certification")],
    ["collection", t("Collection / franchise")],
    ["collections", t("Collections")],
    ["excludeCollections", t("Exclude") + " · " + t("Collections")],
    ["person", t("Person")],
    ["credits", t("People & stories")],
    ["studio", t("Studio / network")],
    ["studioFilters", t("Studio / network")],
    ["firstCharacter", t("Title") + " · " + t("Letter")],
    ["nameStartsWith", t("Title") + " · " + t("Prefix")],
    ["nameStartsWithOrGreater", t("Title") + " · " + t("Minimum")],
    ["nameLessThan", t("Title") + " · " + t("Maximum")],
    ["query", t("Title")],
    ["titleFilters", t("Title") + " · " + t("Rules")],
    ["releasedFilters", t("Release / air date") + " · " + t("Rules")],
  ];
}
export function conditionTitle(rule: Rule, kind?: MediaKind): string {
  if (rule.field === "group")
    return rule.op === "in" ? t("Any") : rule.op === "isNot" ? t("Not") : t("All");
  if (rule.field === "viewer") return t("Viewer activity");
  return browseFields(kind).find(([key]) => `browse:${key}` === rule.field)?.[1] ?? t("Rule");
}
function valueLabel(key: string): string {
  const labels: Record<string, string> = {
    userId: t("Viewer"),
    history: t("Watch history"),
    watched: t("Watched"),
    unwatched: t("Unwatched"),
    inProgress: t("In progress"),
    favorite: t("Favorite"),
    watchlist: t("On watchlist"),
    onWatchlist: t("On watchlist"),
    stateField: t("Activity"),
    stateOperator: t("Comparison"),
    stateValue: t("Value"),
    lastViewedAfter: t("Watched after"),
    lastViewedBefore: t("Watched before"),
    people: t("Person"),
    names: t("Name"),
    kind: t("Role"),
    op: t("Comparison"),
    value: t("Value"),
    unix: t("Date"),
    spanDay: t("Day"),
    name: t("Name"),
  };
  return labels[key] ?? browseFields().find(([field]) => field === key)?.[1] ?? t("Value");
}
// Wire values are separate from translated labels for every finite browse enum.
function enumValues(field: string): string[] | undefined {
  return (
    {
      type: ["movie", "show"],
      releaseGate: ["admitted", "held", "unknown"],
      playable: ["yes", "unknown"],
      debridCached: ["yes", "unknown"],
      qualities: ["4k", "hdr", "dv", "atmos"],
      statuses: ["Returning Series", "Ended", "Canceled", "In Production"],
    } as Record<string, string[]>
  )[field];
}
function enumLabel(field: string, value: string): string {
  const labelField =
    (
      {
        releaseGate: "gate",
        debridCached: "cached",
        qualities: "quality",
        statuses: "status",
      } as Record<string, string>
    )[field] ?? field;
  return catalogValueLabel(labelField, value);
}
function ValueEditor({
  value,
  onChange,
  field = "",
  choices,
}: {
  value: unknown;
  onChange: (v: unknown) => void;
  field?: string;
  choices?: string[];
}) {
  if (
    typeof value === "number" &&
    /^(dateAddedFrom|dateAddedTo|etherAddedFrom|etherAddedTo|unix|lastViewedAfter|lastViewedBefore)$/.test(
      field,
    )
  )
    return (
      <Input
        type="date"
        value={value > 0 ? new Date(value * 1000).toISOString().slice(0, 10) : ""}
        onChange={(e) => onChange(e.target.value ? Date.parse(e.target.value) / 1000 : 0)}
      />
    );

  if (Array.isArray(value))
    return (
      <div className="space-y-2">
        {value.map((v, i) => (
          <div key={i} className="flex items-start gap-2">
            <ValueEditor
              value={v}
              field={field}
              choices={choices}
              onChange={(next) => onChange(value.map((old, j) => (j === i ? next : old)))}
            />
            <Button
              size="sm"
              variant="ghost"
              onClick={() => onChange(value.filter((_, j) => j !== i))}
            >
              {t("Remove")}
            </Button>
          </div>
        ))}
        <Button
          size="sm"
          variant="outline"
          onClick={() => onChange([...value, value.length ? structuredClone(value[0]) : ""])}
        >
          {t("Add")}
        </Button>
      </div>
    );
  if (value && typeof value === "object")
    return (
      <div className="space-y-2">
        {Object.entries(value).map(([k, v]) => (
          <label key={k} className="block space-y-1 text-sm">
            <span>{valueLabel(k)}</span>
            <ValueEditor
              field={k}
              value={v}
              onChange={(next) => onChange({ ...value, [k]: next })}
            />
          </label>
        ))}
      </div>
    );
  const options = choices ?? enumValues(field);
  if (options)
    return (
      <Select value={String(value ?? "")} onChange={(e) => onChange(e.target.value)}>
        {!options.includes(String(value ?? "")) ? (
          <option value={String(value ?? "")}>{value ? String(value) : t("Choose")}</option>
        ) : null}
        {options.map((option) => (
          <option key={option} value={option}>
            {choices ? option : enumLabel(field, option)}
          </option>
        ))}
      </Select>
    );
  if (typeof value === "boolean")
    return (
      <Select value={String(value)} onChange={(e) => onChange(e.target.value === "true")}>
        <option value="true">{t("Yes")}</option>
        <option value="false">{t("No")}</option>
      </Select>
    );
  return (
    <Input
      type={typeof value === "number" ? "number" : "text"}
      value={String(value ?? "")}
      onChange={(e) =>
        onChange(typeof value === "number" ? Number(e.target.value) : e.target.value)
      }
    />
  );
}
export function ExtendedCondition({
  rule,
  onChange,
  onRemove,
  renderChild,
  kind,
}: {
  kind?: MediaKind;
  rule: Rule;
  onChange: (r: Rule) => void;
  onRemove: () => void;
  renderChild: (
    r: Rule,
    i: number,
    onChange: (r: Rule) => void,
    onRemove: () => void,
  ) => React.ReactNode;
}) {
  const children = Array.isArray(rule.value) ? (rule.value as Rule[]) : [];
  const runtime = !kind && (rule.field.includes("Runtime") || rule.field.includes("Runtimes"));
  const convert = (value: unknown, mult: number): unknown =>
    Array.isArray(value)
      ? value.map((v) => convert(v, mult))
      : typeof value === "number"
        ? value * mult
        : value;
  const displayValue = runtime ? convert(rule.value, 1 / 60e9) : rule.value;
  return (
    <div className="space-y-3 rounded-xl border border-border p-3">
      <div className="flex items-center justify-between gap-3">
        <span className="font-medium">{conditionTitle(rule, kind)}</span>
        <Button size="sm" variant="ghost" onClick={onRemove}>
          {t("Remove")}
        </Button>
      </div>
      {rule.field === "group" ? (
        <>
          <Select
            value={rule.op}
            onChange={(e) => onChange({ ...rule, op: e.target.value as Rule["op"] })}
          >
            <option value="is">{t("All")}</option>
            <option value="in">{t("Any")}</option>
            <option value="isNot">{t("Not")}</option>
          </Select>
          {children.map((child, i) =>
            renderChild(
              child,
              i,
              (next) => onChange({ ...rule, value: children.map((v, j) => (i === j ? next : v)) }),
              () => onChange({ ...rule, value: children.filter((_, j) => j !== i) }),
            ),
          )}
          <Select
            aria-label={t("Add condition")}
            value=""
            onChange={(e) => {
              if (e.target.value)
                onChange({
                  ...rule,
                  value: [...children, newBrowseCondition(e.target.value, kind)],
                });
            }}
          >
            <option value="">{t("More conditions")}</option>
            {browseFields(kind).map(([key, label]) => (
              <option key={key} value={key}>
                {label}
              </option>
            ))}
          </Select>
          <Button
            size="sm"
            variant="outline"
            onClick={() =>
              onChange({
                ...rule,
                value: [
                  ...children,
                  kind ? newBrowseCondition("year", kind) : { field: "genre", op: "in", value: [] },
                ],
              })
            }
          >
            {t("Add condition")}
          </Button>
        </>
      ) : (
        <>
          {kind ? (
            <Select
              value={rule.op}
              onChange={(e) => onChange({ ...rule, op: e.target.value as Rule["op"] })}
            >
              <option value="is">{t("is")}</option>
              <option value="isNot">{t("is not")}</option>
            </Select>
          ) : null}
          <ValueEditor
            field={kind ? "" : rule.field.replace("browse:", "")}
            value={displayValue}
            choices={kind?.fields?.find((field) => `browse:${field.key}` === rule.field)?.enum}
            onChange={(value) =>
              onChange({ ...rule, value: runtime ? convert(value, 60e9) : value })
            }
          />
        </>
      )}
    </div>
  );
}
export function newBrowseCondition(key: string, kind?: MediaKind): Rule {
  if (key === "__group") return { field: "group", op: "is", value: [] };
  if (key.startsWith("__")) {
    const todayUnix = Math.floor(Date.now() / 86_400_000) * 86_400;
    const value: Record<string, unknown> =
      key === "__history"
        ? { history: true }
        : key === "__watched"
          ? { watched: true }
          : key === "__unwatched"
            ? { unwatched: true }
            : key === "__progress"
              ? { inProgress: true }
              : key === "__favorite"
                ? { favorite: true }
                : key === "__watchlist"
                  ? { onWatchlist: true }
                  : key === "__lastViewedAfter"
                    ? { lastViewedAfter: todayUnix - 30 * 86_400 }
                    : key === "__lastViewedBefore"
                      ? { lastViewedBefore: todayUnix }
                      : { stateField: "lastRatedAt", stateOperator: ">", stateValue: 0 };
    return {
      field: "viewer",
      op: "is",
      value,
    };
  }
  if (kind)
    return {
      field: `browse:${key}`,
      op: "is",
      value: kind.fields?.find((field) => field.key === key)?.enum?.[0] ?? "",
    };

  const arrays = [
    "qualities",
    "tags",
    "keywords",
    "years",
    "excludeYears",
    "statuses",
    "excludeRatings",
    "excludeRuntimes",
    "languages",
    "countries",
    "excludeCountries",
    "contentRatings",
    "excludeContentRatings",
    "collections",
    "excludeCollections",
  ];
  const numeric =
    (/^(min|max)/.test(key) && key !== "maxContentRating") ||
    /From$|To$/.test(key) ||
    ["collection", "person"].includes(key);
  return {
    field: `browse:${key}` as RuleField,
    op: "is",
    value:
      enumValues(key) && !arrays.includes(key)
        ? enumValues(key)![0]
        : ["anime", "kids"].includes(key)
          ? true
          : arrays.includes(key)
            ? [
                key.includes("Years") ||
                [
                  "years",
                  "excludeRatings",
                  "excludeRuntimes",
                  "collections",
                  "excludeCollections",
                ].includes(key)
                  ? 0
                  : (enumValues(key)?.[0] ?? ""),
              ]
            : key === "credits"
              ? [{ kind: "cast", names: [""] }]
              : ["titleFilters", "studioFilters"].includes(key)
                ? [{ op: "contains", value: "" }]
                : key === "releasedFilters"
                  ? [{ op: ">=", unix: 0, spanDay: true }]
                  : numeric
                    ? 0
                    : "",
  };
}

export function conditionValue(value: unknown, field = ""): string {
  if (typeof value === "number" && /runtime/i.test(field)) return String(value / 60e9);
  if (typeof value === "number" && /dateAdded|etherAdded|unix|lastViewed/.test(field) && value > 0)
    return new Date(value * 1000).toISOString().slice(0, 10);
  if (Array.isArray(value)) return value.map((v) => conditionValue(v, field)).join(", ");
  if (value && typeof value === "object")
    return Object.entries(value)
      .map(([key, v]) => `${valueLabel(key)}: ${conditionValue(v, key)}`)
      .join(" · ");
  if (typeof value === "boolean") return value ? t("Yes") : t("No");
  return String(value ?? "");
}

// Import a transient catalogue view directly into the shared rule model.
export function conditionsFromBrowse(filter: Record<string, unknown>, nested = false): Rule[] {
  const rules: Rule[] = [];
  for (const [key, value] of Object.entries(filter)) {
    if ((key === "type" && !nested) || key === "catalogueSources") continue;
    if (key === "genres") {
      rules.push({ field: "genre", op: "in", value });
      continue;
    }
    if (key === "group") {
      const walk = (value: unknown): Rule => {
        const group = value as Record<string, unknown>;
        const wrap = (children: Rule[], op: Rule["op"]): Rule =>
          children.length === 1 && op !== "isNot"
            ? children[0]!
            : { field: "group", op, value: children };
        if (group.filter)
          return wrap(conditionsFromBrowse(group.filter as Record<string, unknown>, true), "is");
        const { operator, children, ...state } = group;
        if (Object.keys(state).length) return { field: "viewer", op: "is", value: state };
        const converted = ((children ?? []) as unknown[]).map(walk);
        if (operator === "or" && converted.some(emptyGroup))
          return { field: "group", op: "is", value: [] };
        return wrap(
          operator === "not" ? converted : converted.filter((r) => !emptyGroup(r)),
          operator === "or" ? "in" : operator === "not" ? "isNot" : "is",
        );
      };
      const rule = walk(value);
      if (!emptyGroup(rule)) rules.push(rule);
      continue;
    }
    rules.push({ field: `browse:${key}`, op: "is", value });
  }
  return rules;
}

function emptyGroup(rule: Rule): boolean {
  return (
    rule.field === "group" &&
    rule.op !== "isNot" &&
    Array.isArray(rule.value) &&
    rule.value.length === 0
  );
}
