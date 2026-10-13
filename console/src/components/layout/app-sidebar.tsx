// Console navigation, same five views as frontend/src/components/NavPane.vue.
import { Link, useRouterState } from '@tanstack/react-router'
import { Bell, Database, LayoutGrid, Network, Table2, Waves } from 'lucide-react'
import {
  Sidebar,
  SidebarContent,
  SidebarGroup,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuBadge,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarRail,
} from '@/components/ui/sidebar'
import { useObservation } from '@/context/observation'
import { workerCount } from '@/features/cluster/format'
import { useI18n } from '@/lib/i18n'

export function AppSidebar() {
  const { t } = useI18n()
  const { dashboard, overview, workers } = useObservation()
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const s = dashboard.summary
  const items = [
    { to: '/overview', key: 'overview', icon: LayoutGrid, badge: s.total },
    { to: '/tasks', key: 'tasks', icon: Table2, badge: dashboard.total },
    { to: '/sources', key: 'sources', icon: Database, badge: dashboard.sources.length },
    { to: '/workers', key: 'workers', icon: Network, badge: workerCount(overview, workers) },
    { to: '/alerts', key: 'alerts', icon: Bell, badge: s.abnormal + s.failed + s.delayed },
  ] as const

  return (
    <Sidebar collapsible='icon'>
      <SidebarHeader>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton size='lg' className='pointer-events-none'>
              <span className='bg-sidebar-primary text-sidebar-primary-foreground flex size-8 items-center justify-center rounded-lg'>
                <Waves className='size-4' />
              </span>
              <span className='font-semibold'>{t('nav.brand')}</span>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>
      <SidebarContent>
        <SidebarGroup>
          <SidebarMenu>
            {items.map((item) => (
              <SidebarMenuItem key={item.key}>
                <SidebarMenuButton
                  asChild
                  isActive={pathname === item.to}
                  tooltip={t(`nav.${item.key}`)}
                >
                  <Link to={item.to} data-testid={`view-nav-${item.key}`}>
                    <item.icon />
                    <span>{t(`nav.${item.key}`)}</span>
                  </Link>
                </SidebarMenuButton>
                <SidebarMenuBadge data-testid={`view-nav-badge-${item.key}`}>
                  {item.badge}
                </SidebarMenuBadge>
              </SidebarMenuItem>
            ))}
          </SidebarMenu>
        </SidebarGroup>
      </SidebarContent>
      <SidebarRail />
    </Sidebar>
  )
}
