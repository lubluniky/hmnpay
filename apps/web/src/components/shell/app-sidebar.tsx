"use client"

import Link from "next/link"
import { usePathname } from "next/navigation"
import { BookOpenIcon, ChevronsUpDownIcon, LifeBuoyIcon, LogOutIcon, UserIcon } from "lucide-react"

import { Logo, LogoMark, PoweredBy, PROJECT_URL } from "@/components/hmn"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuBadge,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarRail,
  useSidebar,
} from "@/components/ui/sidebar"

import { developerNav, isActive, mainNav, settingsNavItem, type NavItem } from "./nav"
import { NodeStatus } from "./node-status"
import { useDashboard } from "./session"

function NavList({ items, badges }: { items: NavItem[]; badges?: Record<string, number> }) {
  const pathname = usePathname()
  const { setOpenMobile } = useSidebar()
  return (
    <SidebarMenu className="gap-0.5">
      {items.map((item) => {
        const active = isActive(pathname, item)
        const badge = badges?.[item.href]
        return (
          <SidebarMenuItem key={item.href}>
            <SidebarMenuButton
              asChild
              isActive={active}
              tooltip={item.title}
              className="h-8 gap-2.5 px-2.5 text-[13px] text-muted-foreground pointer-coarse:h-10 pointer-coarse:text-sm hover:text-foreground data-active:text-foreground [&_svg]:text-faint data-active:[&_svg]:text-foreground hover:[&_svg]:text-foreground"
            >
              <Link href={item.href} aria-current={active ? "page" : undefined} onClick={() => setOpenMobile(false)}>
                <item.icon />
                <span>{item.title}</span>
              </Link>
            </SidebarMenuButton>
            {badge ? <SidebarMenuBadge className="num right-2 text-[11px] text-muted-foreground">{badge}</SidebarMenuBadge> : null}
          </SidebarMenuItem>
        )
      })}
    </SidebarMenu>
  )
}

export function AppSidebar() {
  const { operator, instance, counts, signOut } = useDashboard()
  // The API knows the operator by email only: that is the identity shown.
  const email = operator.email
  const initial = email.trim().charAt(0).toUpperCase() || "?"
  /** Count shown next to "Payments": payments still open (awaiting / detected / confirming). */
  const openPayments = counts?.open ?? 0
  return (
    <Sidebar collapsible="icon">
      <SidebarHeader className="h-14 flex-row items-center justify-between gap-2 px-4 group-data-[collapsible=icon]:justify-center group-data-[collapsible=icon]:px-0">
        <Link href="/" className="focus-ring rounded-md" aria-label="hmnpay — overview">
          <Logo className="text-[15px] group-data-[collapsible=icon]:hidden" />
          <LogoMark className="hidden size-5 group-data-[collapsible=icon]:block" />
        </Link>
        {instance && (
          <span
            data-slot="version"
            title={`Build ${instance.commit}`}
            className="num max-w-24 truncate rounded-[5px] border border-sidebar-border px-1.5 text-[11px] leading-[18px] tracking-normal text-faint group-data-[collapsible=icon]:hidden"
          >
            {/^\d/.test(instance.version) ? `v${instance.version}` : instance.version}
          </span>
        )}
      </SidebarHeader>

      <SidebarContent className="gap-1 pt-1">
        <SidebarGroup className="px-3 py-1 group-data-[collapsible=icon]:px-2">
          <SidebarGroupContent>
            <NavList items={mainNav} badges={{ "/payments": openPayments }} />
          </SidebarGroupContent>
        </SidebarGroup>

        <SidebarGroup className="px-3 py-1 group-data-[collapsible=icon]:px-2">
          <SidebarGroupLabel className="h-7 px-2.5 text-[11px] font-medium tracking-wide text-faint">Developers</SidebarGroupLabel>
          <SidebarGroupContent>
            <NavList items={developerNav} />
          </SidebarGroupContent>
        </SidebarGroup>

        <SidebarGroup className="mt-auto px-3 py-1 group-data-[collapsible=icon]:px-2">
          <SidebarGroupContent>
            <NavList items={[settingsNavItem]} />
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>

      <SidebarFooter className="gap-2 p-3 group-data-[collapsible=icon]:px-2">
        <NodeStatus />
        <SidebarMenu>
          <SidebarMenuItem>
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <SidebarMenuButton
                  size="lg"
                  className="h-11 gap-2.5 px-1.5 data-[state=open]:bg-sidebar-accent group-data-[collapsible=icon]:justify-center"
                >
                  <span className="flex size-7 shrink-0 items-center justify-center rounded-full bg-muted text-[11px] font-medium text-muted-foreground ring-1 ring-border">
                    {initial}
                  </span>
                  <span className="grid min-w-0 flex-1 text-left leading-tight group-data-[collapsible=icon]:hidden">
                    <span className="truncate text-[13px] font-medium">{email}</span>
                    <span className="truncate text-xs text-muted-foreground">Operator</span>
                  </span>
                  <ChevronsUpDownIcon className="size-3.5! text-faint group-data-[collapsible=icon]:hidden" />
                </SidebarMenuButton>
              </DropdownMenuTrigger>
              <DropdownMenuContent side="top" align="start" className="w-(--radix-dropdown-menu-trigger-width) min-w-56">
                <DropdownMenuLabel className="font-normal">
                  <span className="block truncate text-sm font-medium text-foreground">{email}</span>
                  <span className="block truncate text-xs text-muted-foreground">Operator of this instance</span>
                </DropdownMenuLabel>
                <DropdownMenuSeparator />
                <DropdownMenuGroup>
                  <DropdownMenuItem asChild>
                    <Link href="/settings">
                      <UserIcon />
                      Account
                    </Link>
                  </DropdownMenuItem>
                  <DropdownMenuItem asChild>
                    <a href={`${PROJECT_URL}#readme`} target="_blank" rel="noopener noreferrer">
                      <BookOpenIcon />
                      Documentation
                    </a>
                  </DropdownMenuItem>
                  <DropdownMenuItem asChild>
                    <a href={`${PROJECT_URL}/issues`} target="_blank" rel="noopener noreferrer">
                      <LifeBuoyIcon />
                      Report a problem
                    </a>
                  </DropdownMenuItem>
                </DropdownMenuGroup>
                <DropdownMenuSeparator />
                <DropdownMenuItem onSelect={() => void signOut()}>
                  <LogOutIcon />
                  Log out
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </SidebarMenuItem>
        </SidebarMenu>
        <PoweredBy className="justify-center py-0.5 group-data-[collapsible=icon]:hidden pointer-coarse:py-2" />
      </SidebarFooter>
      <SidebarRail />
    </Sidebar>
  )
}
