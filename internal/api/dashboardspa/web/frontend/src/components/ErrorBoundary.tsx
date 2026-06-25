import { Component, type ErrorInfo, type ReactNode } from 'react';
import { errorMessage } from 'gas-city-dashboard-shared';
import { reportClientError } from '../lib/clientErrorReporting';

interface ErrorBoundaryProps {
  children: ReactNode;
}

interface ErrorBoundaryState {
  crashed: boolean;
}

export class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  override state: ErrorBoundaryState = { crashed: false };

  static getDerivedStateFromError(): ErrorBoundaryState {
    return { crashed: true };
  }

  override componentDidCatch(error: unknown, _errorInfo: ErrorInfo): void {
    void reportClientError({
      component: 'ErrorBoundary',
      operation: 'componentDidCatch',
      message: errorMessage(error),
    });
  }

  override render() {
    if (this.state.crashed) {
      return (
        <main className="max-w-dashboard mx-auto px-4 sm:px-6 lg:px-8 py-12">
          <section className="space-y-4" role="alert">
            <h1 className="text-display font-semibold text-fg">Dashboard view failed.</h1>
            <p className="text-body text-fg-muted">
              The error was reported to the local dashboard log. Refresh to retry this view.
            </p>
          </section>
        </main>
      );
    }

    return this.props.children;
  }
}
