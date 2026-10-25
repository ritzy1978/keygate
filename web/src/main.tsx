import { MutationCache, QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom"
import { ErrorBoundary } from "@/components/error-boundary"
import { AdminLayout, PortalLayout } from "@/components/layout"
import { showToast, ToastBridge, ToastProvider } from "@/components/toast"
import { AuthProvider } from "@/hooks/use-auth"
import { SiteConfigProvider } from "@/hooks/use-site-config"
import { I18nProvider } from "@/i18n"
import AcceptInvitePage from "@/pages/accept-invite"
import AddonsPage from "@/pages/admin/addons"
import AnalyticsPage from "@/pages/admin/analytics"
import APIKeysPage from "@/pages/admin/api-keys"
import AuditPage from "@/pages/admin/audit"
import CustomersPage from "@/pages/admin/customers"
import DashboardPage from "@/pages/admin/dashboard"
import LicensesPage from "@/pages/admin/licenses"
import PlansPage from "@/pages/admin/plans"
import ProductsPage from "@/pages/admin/products"
import ReleasesPage from "@/pages/admin/releases"
import SettingsPage from "@/pages/admin/settings"
import WebhooksPage from "@/pages/admin/webhooks"
import CheckoutSuccessPage from "@/pages/checkout-success"
import LoginPage from "@/pages/login"
import PortalAccountPage from "@/pages/portal/account"
import PortalLicensesPage from "@/pages/portal/licenses"
import SetupPage from "@/pages/setup"
import SignupPage from "@/pages/signup"
import "./index.css"

const queryClient = new QueryClient({
  mutationCache: new MutationCache({
    // A mutation that shows its own errors (translated, in place) sets
    // meta.ownErrors, so they are not shown twice.
    onError: (error, _vars, _ctx, mutation) => {
      if (mutation.meta?.ownErrors) return
      showToast(error instanceof Error ? error.message : "An error occurred")
    },
  }),
  defaultOptions: {
    queries: { retry: 1, refetchOnWindowFocus: false, staleTime: 30_000 },
  },
})

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <I18nProvider>
          <ToastProvider>
            <ToastBridge />
            <SiteConfigProvider>
              <AuthProvider>
                <ErrorBoundary>
                  <Routes>
                    <Route path="/setup" element={<SetupPage />} />
                    <Route path="/login" element={<LoginPage />} />
                    <Route path="/checkout/success" element={<CheckoutSuccessPage />} />
                    <Route path="/accept-invite" element={<AcceptInvitePage />} />
                    <Route path="/signup/:checkoutId" element={<SignupPage />} />

                    {/* Admin */}
                    <Route path="/admin" element={<AdminLayout />}>
                      <Route index element={<DashboardPage />} />
                      <Route path="products" element={<ProductsPage />} />
                      <Route path="plans" element={<PlansPage />} />
                      <Route path="releases" element={<ReleasesPage />} />
                      <Route path="licenses" element={<LicensesPage />} />
                      <Route path="api-keys" element={<APIKeysPage />} />
                      <Route path="webhooks" element={<WebhooksPage />} />
                      <Route path="addons" element={<AddonsPage />} />
                      <Route path="analytics" element={<AnalyticsPage />} />
                      <Route path="audit" element={<AuditPage />} />
                      <Route path="customers" element={<CustomersPage />} />
                      <Route path="settings" element={<SettingsPage />} />
                    </Route>

                    {/* Portal */}
                    <Route path="/portal" element={<PortalLayout />}>
                      <Route index element={<PortalLicensesPage />} />
                      <Route path="account" element={<PortalAccountPage />} />
                    </Route>

                    <Route path="*" element={<Navigate to="/login" replace />} />
                  </Routes>
                </ErrorBoundary>
              </AuthProvider>
            </SiteConfigProvider>
          </ToastProvider>
        </I18nProvider>
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
)
