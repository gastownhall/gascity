import { lazy } from 'react';
import type { FrontendViewDescriptor } from '../types';

export const canvasView: FrontendViewDescriptor = {
  id: 'canvas',
  kind: 'firstParty',
  path: '/canvas',
  nav: { label: 'Canvas', order: 35 },
  element: lazy(() =>
    import('../../routes/BeadsCanvas').then((module) => ({ default: module.BeadsCanvasPage })),
  ),
};
