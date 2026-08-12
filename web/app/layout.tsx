import type { Metadata } from 'next';
import './globals.css';
import { Nav } from '@/components/Nav';
import { AuditProvider } from '@/components/AuditProvider';
import { CommandPalette } from '@/components/CommandPalette';
import { Toaster } from '@/components/Toaster';
import { THEME_INIT_SCRIPT } from '@/lib/theme';

export const metadata: Metadata = {
  title: 'Cloud Asset Auditor',
  description: 'Multi-cloud asset inventory, traffic flow, and network topology.',
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    // suppressHydrationWarning: the script below writes data-theme onto <html>
    // before React runs, so the served markup and the hydrated tree differ by
    // that one attribute on purpose.
    <html lang="en" suppressHydrationWarning>
      <head>
        {/* Render-blocking on purpose. Deferring it would paint one frame in
            the default (dark) theme before switching, which is the flash this
            exists to prevent. */}
        <script dangerouslySetInnerHTML={{ __html: THEME_INIT_SCRIPT }} />
      </head>
      <body>
        <div className="bg-decor" aria-hidden />
        <AuditProvider>
          <div className="shell">
            <Nav />
            <main className="main">{children}</main>
          </div>
          {/* Both live outside .shell: they are fixed-position overlays, and
              nesting them under a scrolling column only invites a containing
              block to appear above them later. */}
          <CommandPalette />
          <Toaster />
        </AuditProvider>
      </body>
    </html>
  );
}
