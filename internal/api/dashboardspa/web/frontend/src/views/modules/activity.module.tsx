import { lazy } from 'react';
import type { FrontendViewDescriptor } from '../types.js';

export const activityView: FrontendViewDescriptor = {
  id: 'activity',
  kind: 'core',
  path: '/activity',
  nav: { label: 'Activity', order: 55 },
  element: lazy(() => import('../../routes/Activity').then((m) => ({ default: m.ActivityPage }))),
};
