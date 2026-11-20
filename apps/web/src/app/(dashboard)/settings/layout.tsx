import { PageContainer, PageHeader } from "@/components/hmn"
import { SettingsNav } from "@/components/shell/settings-nav"

/**
 * Settings frame: one page header, the sub-navigation, and a single column of
 * `SettingsSection` cards (max 760px). Child pages render only their sections.
 */
export default function SettingsLayout({ children }: { children: React.ReactNode }) {
  return (
    <PageContainer>
      <PageHeader title="Settings" description="Configure how this hmnpay instance takes and settles payments." />
      <div className="grid gap-x-12 gap-y-6 lg:grid-cols-[184px_minmax(0,1fr)]">
        <SettingsNav className="lg:sticky lg:top-22 lg:self-start" />
        <div className="min-w-0 max-w-[760px] space-y-6">{children}</div>
      </div>
    </PageContainer>
  )
}
