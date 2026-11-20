import { DashboardShell } from "@/components/shell/shell"

/** Merchant dashboard chrome. Pages render a `PageContainer` + `PageHeader` inside it. */
export default function DashboardLayout({ children }: { children: React.ReactNode }) {
  return <DashboardShell>{children}</DashboardShell>
}
