import type { Metadata } from "next"
import { InboxIcon, LockIcon, PlusIcon, ShieldCheckIcon, TriangleAlertIcon } from "lucide-react"

import {
  Address,
  AddressGroups,
  Amount,
  AssetIcon,
  Callout,
  CopyButton,
  DetailList,
  DetailRow,
  EmptyState,
  Logo,
  LogoMark,
  MerchantAvatar,
  NetworkBadge,
  NetworkIcon,
  PageContainer,
  PageHeader,
  PageSection,
  Panel,
  PaymentStatusBadge,
  SettingsRow,
  SettingsSection,
  Stat,
  StatGroup,
  StatusBadge,
  ThemeToggle,
  TxHash,
  type Tone,
} from "@/components/hmn"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Switch } from "@/components/ui/switch"
import { PAYMENT_STATUSES } from "@/lib/api/types"

import { MotionDemo } from "./motion-demo"
import { SoundBoard } from "./sound-board"
import { WaveDemo } from "./wave-demo"

export const metadata: Metadata = { title: "Design system" }

const TONES: Tone[] = ["neutral", "info", "success", "warning", "danger", "brand"]
const SURFACES = [
  ["sunken", "bg-sunken"],
  ["bg", "bg-background"],
  ["card", "bg-card"],
  ["popover", "bg-popover"],
  ["muted", "bg-muted"],
] as const
const CHARTS = ["bg-chart-1", "bg-chart-2", "bg-chart-3", "bg-chart-4", "bg-chart-5"]

/** Living reference for the hmnpay primitives and tokens. Not linked from the product. */
export default function Page() {
  // Sample values for the address and hash primitives (this page is a styleguide, built only with HMNPAY_DEMO=1).
  const tx = "0x9f2c4be81a07d35c6e9b1f40a8d7c3e25b6f0a91d4c8e73b2a5f6d1c0e9b8a7f"
  const address = "0x7a3f9c2e41b8d05a6e19c4f2b3d8a7e6c5f41b92"

  return (
    <PageContainer>
      <PageHeader
        title="Design system"
        description="Tokens and shared primitives from @/components/hmn. Everything here is the real component."
        actions={
          <>
            <ThemeToggle />
            <Button variant="outline">Secondary</Button>
            <Button>
              <PlusIcon data-icon="inline-start" />
              Primary
            </Button>
          </>
        }
      />

      <div className="space-y-10">
      <PageSection title="Brand">
        <Panel className="flex flex-wrap items-center gap-x-10 gap-y-6 p-6">
          <Logo size="lg" />
          <Logo />
          <Logo size="sm" />
          <LogoMark className="size-8" />
          <LogoMark className="size-4" />
          <MerchantAvatar name="Northwind Digital" />
          <div className="flex items-center gap-3">
            {[16, 20, 24, 32, 40].map((size) => (
              <AssetIcon key={size} size={size} />
            ))}
            <AssetIcon size={24} network={false} />
            <NetworkIcon size={24} />
          </div>
          <NetworkBadge />
          <NetworkBadge asset />
          <NetworkBadge size="sm" />
          <NetworkBadge variant="inline" />
        </Panel>
      </PageSection>

      <PageSection title="Surfaces, tones, charts">
        <Panel className="grid gap-6 p-6 lg:grid-cols-3">
          <div className="grid grid-cols-5 gap-2">
            {SURFACES.map(([name, cls]) => (
              <div key={name}>
                <div className={`h-14 rounded-lg border border-border ${cls}`} />
                <div className="num mt-1.5 text-[11px] text-muted-foreground">{name}</div>
              </div>
            ))}
          </div>
          <div className="flex flex-wrap content-start gap-2">
            {TONES.map((tone) => (
              <StatusBadge key={tone} tone={tone}>
                {tone}
              </StatusBadge>
            ))}
            <StatusBadge tone="success" live>
              live
            </StatusBadge>
            <StatusBadge tone="neutral" hollow>
              hollow
            </StatusBadge>
          </div>
          <div className="grid grid-cols-5 gap-2">
            {CHARTS.map((cls, i) => (
              <div key={cls}>
                <div className={`h-14 rounded-lg ${cls}`} />
                <div className="num mt-1.5 text-[11px] text-muted-foreground">chart-{i + 1}</div>
              </div>
            ))}
          </div>
        </Panel>
      </PageSection>

      <PageSection title="Payment status" description="One badge for all eight states; in-flight states pulse.">
        <Panel className="flex flex-wrap items-center gap-2 p-6">
          {PAYMENT_STATUSES.map((status) => (
            <PaymentStatusBadge key={status} status={status} confirmations={7} required={15} />
          ))}
          <span className="mx-2 h-4 w-px bg-border" />
          {PAYMENT_STATUSES.slice(0, 4).map((status) => (
            <PaymentStatusBadge key={status} status={status} size="sm" />
          ))}
        </Panel>
      </PageSection>

      <PageSection title="Stats">
        <StatGroup>
          <Stat label="Volume (30d)" value={<Amount value="48210.5" />} unit="USDT" delta={{ value: "12.4%", trend: "up" }} hint="vs. previous 30 days" />
          <Stat label="Paid payments" value="312" delta={{ value: "4.1%", trend: "up" }} hint="vs. previous 30 days" />
          <Stat label="Conversion" value={<Amount value="86.2" decimals={1} />} unit="%" delta={{ value: "1.3%", trend: "down" }} hint="paid / created" />
          <Stat label="Average ticket" value={<Amount value="154.52" />} unit="USDT" delta={{ value: "0.0%", trend: "flat" }} />
        </StatGroup>
      </PageSection>

      <div className="grid gap-6 lg:grid-cols-2">
        <PageSection title="Amounts, addresses, hashes">
          <Panel className="p-6">
            <DetailList>
              <DetailRow label="Amount">
                <Amount value="1234.5" asset className="font-medium" />
              </DetailRow>
              <DetailRow label="Dim fraction (tables)">
                <Amount value="1234.5" dimFraction />
              </DetailRow>
              <DetailRow label="Extra precision">
                <Amount value="149.003210" asset />
              </DetailRow>
              <DetailRow label="Signed">
                <Amount value="1" sign="always" asset className="text-success-fg" />
              </DetailRow>
              <DetailRow label="Address">
                <Address value={address} className="-mr-1.5" />
              </DetailRow>
              <DetailRow label="Transaction">
                <TxHash value={tx} className="-mr-1.5" />
              </DetailRow>
              <DetailRow label="Copy button">
                <CopyButton value={address} variant="outline">
                  Copy address
                </CopyButton>
              </DetailRow>
            </DetailList>
            <div className="mt-5 rounded-lg border border-border bg-background/60 p-4">
              <AddressGroups value={address} />
            </div>
          </Panel>
        </PageSection>

        <PageSection title="Callouts and empty state">
          <div className="space-y-3">
            <Callout icon={<ShieldCheckIcon />} title="You can close this page">
              The merchant is notified as soon as the payment is confirmed.
            </Callout>
            <Callout tone="warning" icon={<TriangleAlertIcon />} title="Sweep address is not set">
              Funds stay on deposit addresses until you add one.
            </Callout>
            <Callout tone="danger" icon={<LockIcon />} title="Node unreachable">
              New payments can’t be detected until the RPC connection is back.
            </Callout>
            <EmptyState
              icon={<InboxIcon />}
              title="No payments yet"
              description="Create a payment or share a payment link. It will show up here the moment it’s created."
              action={<Button>Create payment</Button>}
              secondaryAction={<Button variant="outline">Read the docs</Button>}
            />
          </div>
        </PageSection>
      </div>

      <PageSection
        title="Wave transition"
        description="WaveTransition: the one loud moment. For payment confirmed, payment created, link created and setup finished, nowhere else."
      >
        <WaveDemo />
      </PageSection>

      <PageSection
        title="Sound"
        description="Seven cues, one key. Each accompanies a change you can see; none plays before a gesture, in a hidden tab, or when sound is off."
      >
        <SoundBoard />
      </PageSection>

      <PageSection
        title="Motion"
        description="Motion explains a change or answers an action. Shared tokens and primitives from @/components/hmn and globals.css."
      >
        <MotionDemo />
      </PageSection>

      <PageSection title="Settings section" description="The card every settings page is built from.">
        <div className="max-w-[760px] space-y-6">
          <SettingsSection
            title="Merchant name"
            description="Shown to customers at the top of the checkout page and on receipts."
            footer="Use 32 characters or fewer."
            actions={<Button size="sm">Save</Button>}
          >
            <Input defaultValue="Northwind Digital" className="max-w-sm" />
          </SettingsSection>
          <SettingsSection title="Payment rules" description="How strict the gateway is when matching incoming transfers.">
            <div>
              <SettingsRow label="Required confirmations" description="Blocks to wait before a payment counts as paid.">
                <span className="num text-sm">15</span>
              </SettingsRow>
              <SettingsRow label="Accept small underpayments" description="Treat payments short by up to 0.50 USDT as paid.">
                <Switch defaultChecked />
              </SettingsRow>
            </div>
          </SettingsSection>
          <SettingsSection
            tone="danger"
            title="Reset instance"
            description="Deletes every payment, link, key and webhook. Funds on deposit addresses are not touched."
            footer="This can’t be undone."
            actions={
              <Button size="sm" variant="destructive">
                Reset instance
              </Button>
            }
          />
        </div>
      </PageSection>
      </div>
    </PageContainer>
  )
}
