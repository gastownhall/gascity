// Loader for the openclaw channel connectors from their published npm
// packages. No build step: the tarballs ship compiled ESM.
//
// iMessage ships as its own package, `@openclaw/imessage` (split out of
// `openclaw` in 2026.8.x); Telegram is still bundled in `openclaw`
// (dist/extensions/telegram). @openclaw/imessage imports `openclaw/plugin-sdk/*`
// at runtime, so both packages are direct dependencies pinned to the same
// release.
//
// Most of what the bridge needs is on the iMessage plugin's public entry
// modules (runtime-api.js, api.js). One seam — the persistent JSON-RPC client
// used for watch-mode inbound — is NOT re-exported there, only bundled into a
// hash-named chunk under dist/.setup with mangled export aliases.
// resolveChunkExport() finds it by scanning chunk export statements at
// startup, so this keeps working across openclaw releases as long as the
// symbol itself survives.
// (Upstream ask: export createIMessageRpcClient from the plugin API.)

import { readdir, readFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const nodeModules = path.join(here, '..', 'node_modules')
const openclawDist = path.join(nodeModules, 'openclaw', 'dist')
const imessageDist = path.join(nodeModules, '@openclaw', 'imessage', 'dist')

// Loads the connector surface the bridge uses. Everything returned here is
// openclaw's shipped code, imported as-is.
export async function loadIMessageConnector() {
  const entryUrl = (f) => pathToFileURL(path.join(imessageDist, f)).href
  const [runtimeApi, api, createIMessageRpcClient] = await Promise.all([
    import(entryUrl('runtime-api.js')),
    import(entryUrl('api.js')),
    resolveChunkExport(path.join(imessageDist, '.setup'), 'createIMessageRpcClient', 'client-'),
  ])
  return {
    // outbound pipeline: target parsing, markdown handling, chunking, receipts
    sendMessageIMessage: runtimeApi.sendMessageIMessage,
    // startup handshake against the imsg CLI
    probeIMessage: runtimeApi.probeIMessage,
    // persistent JSON-RPC client (send + watch notifications)
    createIMessageRpcClient,
    // canonical conversation-id model
    resolveIMessageInboundConversationId: api.resolveIMessageInboundConversationId,
    normalizeIMessageHandle: api.normalizeIMessageHandle,
    formatIMessageChatTarget: api.formatIMessageChatTarget,
  }
}

// Loads the telegram connector surface. Unlike iMessage, everything the
// bridge needs is on the extension's public entry module — no chunk scanning.
// (Inbound is deliberately NOT loaded from openclaw: their telegram inbound
// lives inside monitorTelegramProvider, the pairing/dispatch layer we don't
// host. The bridge drives Telegram's getUpdates long-poll itself.)
export async function loadTelegramConnector() {
  const runtimeApi = await import(
    pathToFileURL(path.join(openclawDist, 'extensions', 'telegram', 'runtime-api.js')).href
  )
  return {
    // outbound pipeline: markdown -> Telegram HTML, chunking, retries, receipts
    sendMessageTelegram: runtimeApi.sendMessageTelegram,
    // startup handshake: getMe against the (possibly overridden) Bot API root
    probeTelegram: runtimeApi.probeTelegram,
    // forum topics: backs gc's EnsureChildConversation contract
    createForumTopicTelegram: runtimeApi.createForumTopicTelegram,
  }
}

// Finds a function exported (possibly under a mangled alias) from one of the
// hash-named ESM chunks (*.js / *.mjs) directly in `dir`. `hint` orders likely
// filenames first. Exported for test/openclaw-loader.test.mjs.
export async function resolveChunkExport(dir, name, hint) {
  const files = (await readdir(dir)).filter((f) => f.endsWith('.js') || f.endsWith('.mjs'))
  files.sort((a, b) => Number(b.startsWith(hint)) - Number(a.startsWith(hint)))
  for (const f of files) {
    const text = await readFile(path.join(dir, f), 'utf8')
    if (!text.includes(name)) continue
    for (const stmt of text.matchAll(/export\s*\{([^}]*)\}/g)) {
      for (const item of stmt[1].split(',')) {
        const seg = item.trim()
        let key = null
        const asAlias = seg.match(new RegExp(`^${name}\\s+as\\s+(\\w+)$`))
        if (seg === name) key = name
        else if (asAlias) key = asAlias[1]
        else if (new RegExp(`^\\w+\\s+as\\s+${name}$`).test(seg)) key = name
        if (!key) continue
        const mod = await import(pathToFileURL(path.join(dir, f)).href)
        if (typeof mod[key] === 'function') return mod[key]
      }
    }
  }
  throw new Error(`openclaw: could not resolve export ${name} from ${dir}`)
}
