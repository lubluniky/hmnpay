import {
  ArrowLeftRightIcon,
  CableIcon,
  KeyRoundIcon,
  LayoutGridIcon,
  Link2Icon,
  Settings2Icon,
  SlidersHorizontalIcon,
  WalletIcon,
  WebhookIcon,
  type LucideIcon,
} from "lucide-react"

export interface NavItem {
  title: string
  href: string
  icon: LucideIcon
  /** Match nested routes too (`/payments/pay_…`). */
  nested?: boolean
}

export const mainNav: NavItem[] = [
  { title: "Overview", href: "/", icon: LayoutGridIcon },
  { title: "Payments", href: "/payments", icon: ArrowLeftRightIcon, nested: true },
  { title: "Payment links", href: "/payment-links", icon: Link2Icon, nested: true },
]

export const developerNav: NavItem[] = [
  { title: "API keys", href: "/settings/api-keys", icon: KeyRoundIcon, nested: true },
  { title: "Webhooks", href: "/settings/webhooks", icon: WebhookIcon, nested: true },
]

export const settingsNavItem: NavItem = { title: "Settings", href: "/settings", icon: Settings2Icon }

/** Settings sub-navigation, grouped. Rendered by `SettingsNav`. */
export const settingsNav: { label: string; items: NavItem[] }[] = [
  {
    label: "Instance",
    items: [
      { title: "General", href: "/settings", icon: SlidersHorizontalIcon },
      { title: "Wallet", href: "/settings/wallet", icon: WalletIcon, nested: true },
      { title: "Network", href: "/settings/network", icon: CableIcon, nested: true },
    ],
  },
  { label: "Developers", items: developerNav },
]

export function isActive(pathname: string, item: NavItem) {
  if (item.href === "/settings" && item.title === "Settings") {
    // The sidebar "Settings" entry owns /settings/* except the pages promoted to "Developers".
    return pathname.startsWith("/settings") && !developerNav.some((d) => pathname.startsWith(d.href))
  }
  if (pathname === item.href) return true
  return Boolean(item.nested) && pathname.startsWith(`${item.href}/`)
}

/** Breadcrumb trail for the top bar. */
export function trail(pathname: string): { title: string; href?: string }[] {
  if (pathname === "/") return [{ title: "Overview" }]
  if (pathname.startsWith("/payments/")) {
    return [{ title: "Payments", href: "/payments" }, { title: pathname.split("/")[2] ?? "" }]
  }
  if (pathname.startsWith("/settings")) {
    const sub = settingsNav.flatMap((g) => g.items).find((i) => i.href === pathname)
    return [{ title: "Settings", href: "/settings" }, { title: sub?.title ?? "General" }]
  }
  const item = mainNav.find((i) => isActive(pathname, i))
  return [{ title: item?.title ?? "" }]
}
