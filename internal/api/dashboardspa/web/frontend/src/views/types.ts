// Frontend-narrowed alias for the shared ViewDescriptor contract.
// Lives here (not in any single module file) so future module authors
// import the canonical type without coupling to whichever module
// happens to be first in the registry.

import type { ComponentType, LazyExoticComponent } from 'react';
import type { ViewDescriptor } from 'gas-city-dashboard-shared';

export type FrontendViewDescriptor = ViewDescriptor<LazyExoticComponent<ComponentType>>;
