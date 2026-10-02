import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const childProcess = require("node:child_process");
let drainCalls = 0;
childProcess.execFileSync = (_command, args) => {
  if (args[0] === "nudge" && args[1] === "drain") {
    drainCalls += 1;
    return drainCalls === 1 ? "wake once\n" : "";
  }
  return "";
};

let intervalCallback = null;
global.setInterval = (callback) => {
  intervalCallback = callback;
  return { unref() {} };
};
global.clearInterval = () => {};

const handlers = {};
let sendCalls = 0;
const pi = {
  on(name, handler) {
    handlers[name] = handler;
  },
  async sendUserMessage(message, options) {
    sendCalls += 1;
    if (!options || options.deliverAs !== "followUp") {
      throw new Error("Agent is already processing. Specify streamingBehavior");
    }
    if (sendCalls === 1) {
      throw new Error("synthetic provider rejection");
    }
    if (message !== "wake once") {
      throw new Error("retry did not preserve the drained message");
    }
  },
};

require(process.argv[2])(pi);
await handlers.agent_end({}, { cwd: process.cwd() });
if (!intervalCallback) {
  throw new Error("rejected delivery did not arm a retry");
}
await intervalCallback();
if (drainCalls !== 1) {
  throw new Error("drained " + drainCalls + " times; want exactly once");
}
if (sendCalls !== 2) {
  throw new Error("sent " + sendCalls + " times; want rejected attempt plus retry");
}
