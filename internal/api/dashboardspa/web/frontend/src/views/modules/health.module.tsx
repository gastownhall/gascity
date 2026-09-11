// Health is a core view: always mounted, never opt-out-able. The lazy
// import keeps the Health route's chunk out of the default first-paint
// bundle even though it's core — the route only renders when the operator
// navigates to /health, which is rare.

import { lazy } from 'react';
import type { FrontendViewDescriptor } from '../types';

export const healthView: FrontendViewDescriptor = {
  id: 'health',
  kind: 'core',
  path: '/health',
  nav: { label: 'Health', order: 60 },
  element: lazy(() => import('../../routes/Health').then((m) => ({ default: m.HealthPage }))),
};
