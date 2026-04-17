import { api, cityScope } from "./api";
import { byId, clear, el } from "./util/dom";
import { closeOutput, openOutput } from "./ui";

interface PaletteCommand {
  category: string;
  desc: string;
  name: string;
  run: () => Promise<void> | void;
}

export function installCommandPalette(deps: { refreshAll: () => Promise<void> }): void {
  const overlay = byId("command-palette-overlay");
  const input = byId<HTMLInputElement>("command-palette-input");
  const results = byId("command-palette-results");
  const openBtn = byId("open-palette-btn");
  if (!overlay || !input || !results || !openBtn) return;
  const paletteOverlay = overlay;
  const paletteInput = input;
  const paletteResults = results;
  const paletteOpenBtn = openBtn;

  let commands: PaletteCommand[] = [];
  let visible: PaletteCommand[] = [];
  let selected = 0;

  function buildCommands(): PaletteCommand[] {
    const city = cityScope();
    const read = async (label: string, promise: Promise<unknown>): Promise<void> => {
      const data = await promise;
      openOutput(label, JSON.stringify(data, null, 2));
    };
    return [
      { name: "refresh", desc: "Refresh all panels", category: "Dashboard", run: () => deps.refreshAll() },
      { name: "supervisor health", desc: "Show supervisor health JSON", category: "Supervisor", run: () => read("health", api.GET("/health")) },
      { name: "city list", desc: "Show managed cities JSON", category: "Supervisor", run: () => read("cities", api.GET("/v0/cities")) },
      { name: "global events", desc: "Show recent supervisor events JSON", category: "Supervisor", run: () => read("events", api.GET("/v0/events", {
        params: { query: { since: "1h" } },
      })) },
      ...(city ? [
        { name: "new issue", desc: "Open the issue creation modal", category: "Work", run: () => click("new-issue-btn") },
        { name: "compose mail", desc: "Open the compose mail form", category: "Mail", run: () => click("compose-mail-btn") },
        { name: "new convoy", desc: "Open the convoy creation form", category: "Convoys", run: () => click("new-convoy-btn") },
        {
          name: "status",
          desc: "Show current city status JSON",
          category: "Status",
          run: () => read("status", api.GET("/v0/city/{cityName}/status", { params: { path: { cityName: city } } })),
        },
        {
          name: "agent list",
          desc: "Show current sessions JSON",
          category: "Status",
          run: () => read("sessions", api.GET("/v0/city/{cityName}/sessions", {
            params: { path: { cityName: city }, query: { state: "active", peek: "true" } },
          })),
        },
        {
          name: "convoy list",
          desc: "Show current convoys JSON",
          category: "Convoys",
          run: () => read("convoys", api.GET("/v0/city/{cityName}/convoys", {
            params: { path: { cityName: city }, query: { limit: 200 } },
          })),
        },
        {
          name: "mail inbox",
          desc: "Show current mail JSON",
          category: "Mail",
          run: () => read("mail", api.GET("/v0/city/{cityName}/mail", {
            params: { path: { cityName: city }, query: { status: "all", limit: 200 } },
          })),
        },
        {
          name: "rig list",
          desc: "Show rig JSON",
          category: "Rigs",
          run: () => read("rigs", api.GET("/v0/city/{cityName}/rigs", {
            params: { path: { cityName: city }, query: { git: "true" } },
          })),
        },
        {
          name: "list",
          desc: "Show open and in-progress beads JSON",
          category: "Beads",
          run: async () => {
            const [open, progress] = await Promise.all([
              api.GET("/v0/city/{cityName}/beads", { params: { path: { cityName: city }, query: { status: "open", limit: 500 } } }),
              api.GET("/v0/city/{cityName}/beads", { params: { path: { cityName: city }, query: { status: "in_progress", limit: 500 } } }),
            ]);
            openOutput("beads", JSON.stringify({
              open: open.data?.items ?? [],
              in_progress: progress.data?.items ?? [],
            }, null, 2));
          },
        },
      ] : []),
      { name: "close output", desc: "Hide the output panel", category: "Dashboard", run: () => closeOutput() },
    ].filter((command) => typeof command.run === "function");
  }

  function render(): void {
    clear(paletteResults);
    const query = paletteInput.value.trim().toLowerCase();
    commands = buildCommands();
    visible = commands.filter((command) =>
      query === "" ||
      command.name.includes(query) ||
      command.desc.toLowerCase().includes(query) ||
      command.category.toLowerCase().includes(query),
    );
    if (selected >= visible.length) selected = 0;
    if (visible.length === 0) {
      paletteResults.append(el("div", { class: "command-palette-empty" }, ["No matching commands"]));
      return;
    }
    visible.forEach((command, index) => {
      const item = el("button", {
        class: `command-item${index === selected ? " selected" : ""}`,
        type: "button",
      }, [
        el("span", { class: "command-name" }, [`gt ${command.name}`]),
        el("span", { class: "command-desc" }, [command.desc]),
        el("span", { class: "command-category" }, [command.category]),
      ]);
      item.addEventListener("click", () => {
        void execute(index);
      });
      paletteResults.append(item);
    });
  }

  function open(): void {
    paletteOverlay.classList.add("open");
    paletteInput.value = "";
    selected = 0;
    render();
    paletteInput.focus();
  }

  function close(): void {
    paletteOverlay.classList.remove("open");
  }

  async function execute(index: number): Promise<void> {
    const command = visible[index];
    close();
    if (!command) return;
    await command.run();
  }

  paletteOpenBtn.addEventListener("click", () => open());
  paletteOverlay.addEventListener("click", (event) => {
    if (event.target === paletteOverlay) close();
  });
  paletteInput.addEventListener("input", () => render());
  paletteInput.addEventListener("keydown", (event) => {
    if (event.key === "ArrowDown") {
      selected = Math.min(selected + 1, Math.max(visible.length - 1, 0));
      render();
      event.preventDefault();
      return;
    }
    if (event.key === "ArrowUp") {
      selected = Math.max(selected - 1, 0);
      render();
      event.preventDefault();
      return;
    }
    if (event.key === "Enter") {
      void execute(selected);
      event.preventDefault();
      return;
    }
    if (event.key === "Escape") {
      close();
    }
  });
  document.addEventListener("keydown", (event) => {
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
      event.preventDefault();
      if (paletteOverlay.classList.contains("open")) {
        close();
      } else {
        open();
      }
    }
  });
}

function click(id: string): void {
  byId<HTMLElement>(id)?.click();
}
