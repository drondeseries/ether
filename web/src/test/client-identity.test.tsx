import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Indexer } from "../lib/types";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 4000 };
const DEFAULT_UA = "Ether/dev (+https://github.com/braedonsaunders/ether)";

describe("Sources › Indexers client identity", () => {
  it("shows the default Ether placeholder and warning note, and saves a free-text user agent", async () => {
    const { fake } = renderApp("/sources/indexers");
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    const uaInput = screen.getByLabelText("User agent");
    expect(uaInput).toHaveAttribute("placeholder", DEFAULT_UA);
    expect(
      screen.getByText(
        "Leave empty to identify as Ether. Indexers may block or ban clients that pretend to be another app.",
      ),
    ).toBeInTheDocument();

    fireEvent.change(uaInput, { target: { value: "CustomClient/2.0" } });
    const bar = screen.getByRole("region", { name: "Unsaved changes" });
    fireEvent.click(within(bar).getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/clientIdentity")).toHaveLength(1), WAIT);
    expect(calls(fake, "PUT", "/config/clientIdentity")[0]!.body).toEqual({
      userAgent: "CustomClient/2.0",
      headers: {},
    });
  });

  it("overrides the user agent on one indexer", async () => {
    const { fake } = renderApp("/sources/indexers");
    fireEvent.click(await screen.findByRole("button", { name: /Add indexer/ }, WAIT));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Newznab or Torznab URL…" }));
    const drawer = await screen.findByRole("dialog", { name: "New indexer" }, WAIT);
    fireEvent.change(within(drawer).getByLabelText("Name"), { target: { value: "Idx" } });
    fireEvent.change(within(drawer).getByLabelText("URL"), {
      target: { value: "https://idx.example" },
    });
    fireEvent.change(within(drawer).getByLabelText("User agent (optional)"), {
      target: { value: "IndexerClient/1.0" },
    });
    fireEvent.click(within(drawer).getByRole("button", { name: "Save indexer" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/indexers")).toHaveLength(1), WAIT);
    const body = calls(fake, "PUT", "/config/indexers")[0]!.body as Indexer[];
    expect(body.at(-1)).toMatchObject({ name: "Idx", userAgent: "IndexerClient/1.0" });
  });

  it("refuses two rows with the same header name", async () => {
    const { fake } = renderApp("/sources/indexers");
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    fireEvent.click(screen.getByRole("button", { name: "More options: Client identity" }));
    fireEvent.click(screen.getByRole("button", { name: "Add header" }));
    fireEvent.click(screen.getByRole("button", { name: "Add header" }));
    fireEvent.change(screen.getAllByLabelText("Header name")[0]!, {
      target: { value: "X-Test" },
    });
    fireEvent.change(screen.getAllByLabelText("Header name")[1]!, {
      target: { value: "X-Test" },
    });
    // The duplicate keystroke is refused rather than silently collapsing.
    expect((screen.getAllByLabelText("Header name")[1] as HTMLInputElement).value).toBe("");
    fireEvent.change(screen.getAllByLabelText("Value")[0]!, { target: { value: "one" } });
    const bar = screen.getByRole("region", { name: "Unsaved changes" });
    fireEvent.click(within(bar).getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/clientIdentity")).toHaveLength(1), WAIT);
    // The unfilled row is a placeholder, not a header.
    expect(calls(fake, "PUT", "/config/clientIdentity")[0]!.body).toMatchObject({
      headers: { "X-Test": "one" },
    });
  });

  it("renders custom headers as secret rows", async () => {
    const { fake } = renderApp("/sources/indexers");
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    fireEvent.click(screen.getByRole("button", { name: "More options: Client identity" }));
    fireEvent.click(screen.getByRole("button", { name: "Add header" }));
    fireEvent.change(screen.getByLabelText("Header name"), { target: { value: "X-Api-Client" } });
    fireEvent.change(screen.getByLabelText("Value"), { target: { value: "s3cret" } });
    const bar = screen.getByRole("region", { name: "Unsaved changes" });
    fireEvent.click(within(bar).getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/clientIdentity")).toHaveLength(1), WAIT);
    expect(calls(fake, "PUT", "/config/clientIdentity")[0]!.body).toMatchObject({
      headers: { "X-Api-Client": "s3cret" },
    });
  });

  it("counts inheriting indexers and names the backend exceptions", async () => {
    renderApp("/sources/indexers");
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    // The mock has three Newznab/Torznab indexers and one Stremio addon.
    expect(screen.getByText("All 3 indexers inherit this policy")).toBeInTheDocument();
    expect(
      screen.getByText("Built-in sites and Stremio addons use their own identity."),
    ).toBeInTheDocument();
  });

  it("counts overrides and follows an indexer edit", async () => {
    renderApp("/sources/indexers", {}, (fake) => {
      fake.state.config.indexers!.find((ix) => ix.id === "i1")!.userAgent = "CustomIdx/1.0";
    });
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    expect(
      screen.getByText("2 of 3 indexers inherit this policy; 1 has its own identity"),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Edit Indexer One" }));
    const drawer = await screen.findByRole("dialog", { name: "Indexer One" }, WAIT);
    fireEvent.change(within(drawer).getByLabelText("User agent (optional)"), {
      target: { value: "" },
    });
    fireEvent.click(within(drawer).getByRole("button", { name: "Save indexer" }));
    await waitFor(
      () => expect(screen.getByText("All 3 indexers inherit this policy")).toBeInTheDocument(),
      WAIT,
    );
  });

  it("omits the count when no indexer can inherit", async () => {
    renderApp("/sources/indexers", {}, (fake) => {
      fake.state.config.indexers = fake.state.config.indexers!.filter(
        (ix) => ix.kind === "stremio",
      );
    });
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    expect(screen.queryByText(/inherit this policy/)).toBeNull();
  });

  it("labels the disclosure Fewer options while it is open", async () => {
    renderApp("/sources/indexers");
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    const more = screen.getByRole("button", { name: "More options: Client identity" });
    fireEvent.click(more);
    expect(more).toHaveTextContent("Fewer options");
    fireEvent.click(more);
    expect(more).toHaveTextContent("More options");
  });

  it("shows both custom header fields inside More options", async () => {
    renderApp("/sources/indexers");
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    fireEvent.click(screen.getByRole("button", { name: "More options: Client identity" }));
    fireEvent.click(screen.getByRole("button", { name: "Add header" }));
    expect(screen.getByLabelText("Header name")).toBeInTheDocument();
    expect(screen.getByLabelText("Value")).toBeInTheDocument();
  });
});
