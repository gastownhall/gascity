import { describe, expect, it } from "vitest";
import { extractMailForTest } from "./comms";

describe("comms extractMail", () => {
  it("parses a mail.sent envelope into a sender->recipient edge", () => {
    const edge = extractMailForTest({
      type: "mail.sent",
      ts: "2026-01-01T00:00:00Z",
      payload: {
        rig: "",
        message: {
          from: "controller",
          to: "gastown.mayor",
          subject: "Dolt health advisory",
          id: "gc-1",
          created_at: "2026-01-01T00:00:01Z",
        },
      },
    });
    expect(edge).toEqual({
      from: "controller",
      to: "gastown.mayor",
      subject: "Dolt health advisory",
      id: "gc-1",
      ts: "2026-01-01T00:00:01Z",
    });
  });

  it("accepts mail.replied and falls back to envelope ts when message has no created_at", () => {
    const edge = extractMailForTest({
      type: "mail.replied",
      ts: "2026-01-02T00:00:00Z",
      payload: { message: { from: "human", to: "human" } },
    });
    expect(edge?.from).toBe("human");
    expect(edge?.ts).toBe("2026-01-02T00:00:00Z");
    expect(edge?.subject).toBe("");
  });

  it("ignores non-mail event types and malformed payloads", () => {
    expect(extractMailForTest({ type: "mail.read", payload: { message: { from: "a", to: "b" } } })).toBeNull();
    expect(extractMailForTest({ type: "bead.updated", payload: {} })).toBeNull();
    expect(extractMailForTest({ type: "mail.sent", payload: {} })).toBeNull();
    expect(extractMailForTest({ type: "mail.sent", payload: { message: { from: "a" } } })).toBeNull();
  });
});
