import { useMemo } from 'react';
import type { RunDiffComparison, RunDiffResponse } from 'gas-city-dashboard-shared';
import {
  Diff,
  Hunk,
  parseDiff,
  type FileData,
  type GutterOptions,
  type HunkData,
} from 'react-diff-view';

interface RunDiffPanelProps {
  diff: RunDiffResponse;
}

export function RunDiffPanel({ diff }: RunDiffPanelProps) {
  switch (diff.kind) {
    case 'path_unknown':
      // The run bead carries no work_dir / cwd / rig_root, so there is no
      // folder to diff against. This is expected for rig-store run beads the
      // supervisor doesn't yet stamp with `gc.work_dir` (gascity-dashboard-j7np,
      // upstream-gated) — present it as a calm absence, not a failure.
      return (
        <div className="space-y-1">
          <p className="text-body text-fg-muted italic">No diff available for this run.</p>
          <p className="text-label text-fg-faint">
            The run did not record a work_dir, so there is no work tree to compare.
          </p>
        </div>
      );
    case 'not_git':
      return (
        <p className="text-body text-fg-muted italic">Execution folder is not a git work tree.</p>
      );
    case 'error':
      return (
        <p className="text-body text-accent" role="alert">
          {diff.error}
        </p>
      );
    case 'ok':
      return <RenderableDiff diff={diff} />;
  }
}

function RenderableDiff({ diff }: { diff: Extract<RunDiffResponse, { kind: 'ok' }> }) {
  const files = useMemo(() => parsePatch(diff.patch), [diff.patch]);

  return (
    <section>
      <div className="flex items-baseline justify-between gap-4 flex-wrap">
        <h3 className="text-body font-semibold text-fg">Local Changes</h3>
        <span className="text-label uppercase tracking-wider text-fg-faint tnum">
          {diff.changedFiles.length} changed file{diff.changedFiles.length === 1 ? '' : 's'}
        </span>
      </div>
      {diff.rootPath.kind === 'known' && (
        <p className="mt-1 text-label text-fg-faint break-all">{diff.rootPath.path}</p>
      )}
      <p className="mt-2 text-label uppercase tracking-wider text-fg-muted">
        {comparisonText(diff.comparison)}
      </p>
      {files.length === 0 ? (
        <p className="mt-5 text-body text-fg-muted italic">
          No renderable patch in this work tree.
        </p>
      ) : (
        <div className="formula-run-diff-view mt-5 space-y-3">
          {files.map((file) => (
            <DiffFile
              key={`${file.oldRevision}:${file.newRevision}:${filePath(file)}`}
              file={file}
            />
          ))}
        </div>
      )}
      {diff.truncated && (
        <p className="mt-3 text-label uppercase tracking-wider text-fg-faint">
          Diff truncated at the backend output cap.
        </p>
      )}
    </section>
  );
}

function DiffFile({ file }: { file: FileData }) {
  const counts = countChanges(file.hunks);
  return (
    <details className="border-y border-rule py-2" open>
      <summary className="cursor-pointer list-none text-label uppercase tracking-wider text-fg-muted">
        <span className="font-medium normal-case tracking-normal text-body text-fg">
          {filePath(file)}
        </span>
        <span className="ml-3 tnum text-fg-faint">
          +{counts.additions} -{counts.deletions}
        </span>
      </summary>
      {file.hunks.length === 0 || file.isBinary ? (
        <p className="mt-3 text-body text-fg-muted italic">No textual hunks.</p>
      ) : (
        <div className="mt-3 overflow-auto">
          <Diff
            viewType="unified"
            diffType={file.type}
            hunks={file.hunks}
            renderGutter={renderGitGutter}
          >
            {(hunks) => hunks.map((hunk) => <Hunk key={hunkKey(hunk)} hunk={hunk} />)}
          </Diff>
        </div>
      )}
    </details>
  );
}

function parsePatch(patch: string): FileData[] {
  if (patch.trim().length === 0) return [];
  try {
    return parseDiff(patch, { nearbySequences: 'zip' });
  } catch {
    return [];
  }
}

function renderGitGutter({ change, side, renderDefault }: GutterOptions) {
  if (change.type === 'insert' && side === 'old') {
    return (
      <span className="diff-gutter-sign" aria-hidden>
        +
      </span>
    );
  }
  if (change.type === 'delete' && side === 'new') {
    return (
      <span className="diff-gutter-sign" aria-hidden>
        -
      </span>
    );
  }
  return renderDefault();
}

function comparisonText(comparison: RunDiffComparison): string {
  if (comparison.kind === 'upstream') {
    return `Compared with ${comparison.ref} at ${comparison.mergeBase.slice(0, 12)}.`;
  }
  if (comparison.kind === 'head' && comparison.reason === 'no_upstream') {
    return 'No upstream branch is configured; showing changes relative to HEAD plus untracked files.';
  }
  if (comparison.kind === 'head') {
    return 'Upstream comparison failed; showing changes relative to HEAD plus untracked files.';
  }
  return 'Comparison unavailable.';
}

function filePath(file: FileData): string {
  const oldPath = trimDiffPrefix(file.oldPath);
  const newPath = trimDiffPrefix(file.newPath);
  if (file.type === 'delete') return oldPath;
  if (file.type === 'rename' && oldPath !== newPath) return `${oldPath} -> ${newPath}`;
  return newPath || oldPath;
}

function trimDiffPrefix(filePath: string): string {
  return filePath.replace(/^[ab]\//, '');
}

function countChanges(hunks: HunkData[]): { additions: number; deletions: number } {
  let additions = 0;
  let deletions = 0;
  for (const hunk of hunks) {
    for (const change of hunk.changes) {
      if (change.type === 'insert') additions += 1;
      if (change.type === 'delete') deletions += 1;
    }
  }
  return { additions, deletions };
}

function hunkKey(hunk: HunkData): string {
  return `${hunk.oldStart}:${hunk.newStart}:${hunk.content}`;
}
