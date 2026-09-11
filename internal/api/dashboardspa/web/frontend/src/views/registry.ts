// Frontend view registry — the single list iterated by App.tsx and
// Header.tsx. PR-A contains only `health`; later PRs add the remaining
// first-party views per PRD §3.
//
// The list is `readonly` so views cannot be appended at runtime. New
// views are registered by editing this file — the compile-time edit IS
// the design-review checkpoint, premortem #6.

import { activityView } from './modules/activity.module.js';
import { healthView } from './modules/health.module.js';
import type { FrontendViewDescriptor } from './types.js';

export const ALL_VIEWS: ReadonlyArray<FrontendViewDescriptor> = [activityView, healthView];

export type { FrontendViewDescriptor };
