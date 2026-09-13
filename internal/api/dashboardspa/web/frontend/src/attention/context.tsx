import { createContext, useContext, useMemo, type ReactNode } from 'react';
import { composeAttention, type AttentionContributor, type AttentionModel } from './compose';

const EMPTY_ATTENTION = composeAttention([]);
const AttentionContext = createContext<AttentionModel>(EMPTY_ATTENTION);

export function AttentionProvider({
  contributors,
  topLimit,
  children,
}: {
  contributors: readonly AttentionContributor[];
  topLimit?: number;
  children: ReactNode;
}) {
  const model = useMemo(
    () =>
      topLimit === undefined
        ? composeAttention(contributors)
        : composeAttention(contributors, { topLimit }),
    [contributors, topLimit],
  );

  return <AttentionContext.Provider value={model}>{children}</AttentionContext.Provider>;
}

export function useAttentionModel(): AttentionModel {
  return useContext(AttentionContext);
}
