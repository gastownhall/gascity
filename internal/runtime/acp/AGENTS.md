# ACP Startup

- The startup sentinel reports alive before the ACP handshake completes.
  Seed ownership through `runtime.SplitEnvForMetaSeed` before publishing the
  sentinel, not after handshake. Child environment alone cannot satisfy
  `GetMeta`, which reads private sidecars.
- A cancelled startup owns its reservation until its process and writes drain.
  `Stop` cancels and waits for that completion. Releasing the name early lets
  old socket or metadata cleanup destroy a retry's state.
- The per-name lifecycle file is a kernel-held coordination lock, not a process
  status record. Keep its inode across starts; unlinking it splits contenders
  across different locks. It also fences different Provider instances while a
  failed startup has no live socket but still has cleanup pending.
- Closing the Unix listener unlinks its socket. Do not unlink the socket again
  after Close: another provider may already have bound a replacement.
- A dead in-memory connection can coexist with a live replacement owned by
  another provider. Its stale `Stop` must not remove the replacement's metadata.
- Release the lifecycle lock after committing the real connection, before the
  initial nudge. A blocked stdin write needs Stop to acquire that lock and close
  the pipe. Bind initial delivery to the committed connection, not another
  name lookup that could select a replacement after a concurrent stop/retry.
- Handshake cancellation must close the startup-owned stdin pipe as well as
  cancel response waits. Otherwise a backpressured write holds the lifecycle
  lock indefinitely and prevents Stop from killing the process. The handshake's
  `context.AfterFunc` callback closes that pipe without locks; disarm and join
  it before transferring the connection so late cancellation cannot close a
  successful session or interfere with retry cleanup.
