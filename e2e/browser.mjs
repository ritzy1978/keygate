// Browser walk-through of self sign-up and upgrade-keeping-the-key,
// against a Keygate container started by .github/workflows/browser-e2e.yml.
// Screenshots go to e2e-shots/. Stripe is not reachable from the test,
// so the paid upgrade is simulated with a signed checkout.session.completed
// webhook, exactly as Stripe would send it.
import { execSync } from "node:child_process"
import crypto from "node:crypto"
import fs from "node:fs"
import { chromium } from "playwright"

const BASE = "http://localhost:9000"
const OUT = "e2e-shots"
const WEBHOOK_SECRET = "whsec_e2e"
const CUSTOMER = "customer@example.com"
fs.mkdirSync(OUT, { recursive: true })

const results = []
const check = (name, ok, detail = "") => {
  results.push({ name, ok, detail })
  console.log(`${ok ? "PASS" : "FAIL"} ${name}${detail ? ` — ${detail}` : ""}`)
}
const shot = async (page, name) => {
  await page.waitForTimeout(600)
  await page.screenshot({ path: `${OUT}/${name}.png`, fullPage: true })
  console.log(`shot ${name}`)
}

async function api(ctx, method, path, data) {
  const r = await ctx.request.fetch(`${BASE}/api/v1${path}`, { method, data })
  const body = await r.json().catch(() => ({}))
  if (!r.ok()) throw new Error(`${method} ${path} -> ${r.status()} ${JSON.stringify(body)}`)
  return body.data
}

// The newest login code mailed to email. With no SMTP configured,
// Keygate logs it (JSON lines) instead of sending it.
async function codeFor(email) {
  for (let i = 0; i < 30; i++) {
    const lines = execSync("docker logs kg 2>&1").toString().split("\n")
    for (const line of lines.reverse()) {
      try {
        const j = JSON.parse(line)
        if (j.email === email && /^\d{6}$/.test(j.code ?? "")) return j.code
      } catch {}
    }
    await new Promise((r) => setTimeout(r, 1000))
  }
  throw new Error(`no code logged for ${email}`)
}

async function signUp(page, checkoutId, email, tag) {
  await page.goto(`${BASE}/pay/${checkoutId}`)
  await page.waitForURL(/\/signup\//)
  check(`${tag}: /pay redirects to the sign-up form`, page.url().includes(`/signup/${checkoutId}`), page.url())
  await page.getByPlaceholder("you@example.com").fill(email)
  const consent = page.locator('input[type="checkbox"]')
  if (await consent.count()) await consent.check()
  await shot(page, `${tag}-a-form`)
  await page.getByRole("button", { name: "Send confirmation code" }).click()
  await page.getByText(`Code sent to ${email}`).waitFor()
  await page.getByPlaceholder("000000").fill(await codeFor(email))
  await shot(page, `${tag}-b-code`)
  await page.getByRole("button", { name: "Get my license" }).click()
  await page.getByRole("link", { name: "Open my account" }).waitFor()
  await shot(page, `${tag}-c-done`)
}

const browser = await chromium.launch()
try {
  // ── Operator: first-run setup, sign in, catalogue ──
  const admin = await browser.newContext({ viewport: { width: 1280, height: 900 } })
  const setup = await api(admin, "POST", "/setup/initialize", {
    admin_email: "admin@example.com",
    admin_name: "Admin",
    site_name: "LiveKit Dashboard Licensing",
    product_name: "LiveKit Dashboard",
    product_slug: "livekit-dashboard",
    product_type: "hybrid",
  })
  const productId = setup.product.id
  await api(admin, "POST", "/auth/dev-login", { email: "admin@example.com", name: "Admin" })
  const community = await api(admin, "POST", "/admin/plans", {
    product_id: productId,
    name: "Community",
    slug: "community",
    license_type: "subscription",
    billing_interval: "month",
    max_activations: 1,
    self_signup: true,
    signup_daily_cap: 100,
    signup_consent_text: "Email me LiveKit Dashboard news and offers",
  })
  const premium = await api(admin, "POST", "/admin/plans", {
    product_id: productId,
    name: "Premium",
    slug: "premium",
    license_type: "subscription",
    billing_interval: "month",
    max_activations: 1,
    stripe_price_id: "price_e2ePremium",
  })
  check("plan saved with self sign-up settings", community.self_signup === true && community.signup_daily_cap === 100)
  let refused = ""
  try {
    await api(admin, "POST", "/admin/plans", {
      product_id: productId,
      name: "Bad",
      slug: "bad",
      license_type: "subscription",
      billing_interval: "month",
      stripe_price_id: "price_e2eBad",
      self_signup: true,
    })
  } catch (e) {
    refused = String(e)
  }
  check("self sign-up refused on a priced plan", refused.includes("400"), refused.slice(0, 120))

  const adminPage = await admin.newPage()
  await adminPage.goto(`${BASE}/admin/plans`)
  await adminPage.getByText("Community").first().waitFor()
  await shot(adminPage, "01-admin-plans")
  try {
    // Row actions: copy link, edit, delete.
    await adminPage.getByRole("row", { name: /Community/ }).getByRole("button").nth(1).click()
    await adminPage.getByText("Self sign-up").first().waitFor({ timeout: 5000 })
    await shot(adminPage, "02-admin-plan-self-signup-settings")
    await adminPage.keyboard.press("Escape")
  } catch (e) {
    console.log(`(could not open the plan editor for a screenshot: ${e.message})`)
  }

  // ── Customer: free sign-up ──
  const customer = await browser.newContext({ viewport: { width: 1280, height: 900 } })
  const page = await customer.newPage()
  await signUp(page, community.checkout_id, CUSTOMER, "03-signup")
  await page.getByRole("link", { name: "Open my account" }).click()
  await page.waitForURL(/\/portal/)
  await page.getByRole("button", { name: "Upgrade" }).first().waitFor()
  await shot(page, "04-portal-community-with-upgrade")

  let lic = (await api(customer, "GET", "/portal/licenses")).licenses[0]
  const keyBefore = lic.license_key
  check("free licence issued on Community", lic.plan?.name === "Community", lic.plan?.name)
  check("marketing consent recorded", lic.marketing_consent === true)
  check("signed in to the portal after sign-up", !!keyBefore)

  // ── Customer: upgrade dialog ──
  await page.getByRole("button", { name: "Upgrade" }).first().click()
  await page.getByRole("dialog").getByText("Premium").waitFor()
  await shot(page, "05-upgrade-dialog")
  check("upgrade dialog offers Premium", true)
  await page.keyboard.press("Escape")

  // ── Stripe: the customer pays; Stripe sends checkout.session.completed ──
  const now = Math.floor(Date.now() / 1000)
  const payload = JSON.stringify({
    id: "evt_e2e_upgrade",
    object: "event",
    api_version: "2024-06-20",
    type: "checkout.session.completed",
    livemode: false,
    created: now,
    data: {
      object: {
        id: "cs_test_e2e_upgrade",
        object: "checkout.session",
        customer: "cus_e2e",
        subscription: "sub_e2e",
        payment_status: "paid",
        mode: "subscription",
        created: now,
        customer_details: { email: CUSTOMER },
        metadata: {
          kind: "upgrade",
          license_id: lic.id,
          plan_id: premium.id,
          product_id: productId,
          license_type: "subscription",
        },
      },
    },
  })
  const sig = crypto.createHmac("sha256", WEBHOOK_SECRET).update(`${now}.${payload}`).digest("hex")
  for (const attempt of ["first delivery", "Stripe retry"]) {
    const r = await customer.request.post(`${BASE}/api/v1/webhook/stripe`, {
      data: payload,
      headers: { "Content-Type": "application/json", "Stripe-Signature": `t=${now},v1=${sig}` },
    })
    check(`webhook accepted (${attempt})`, r.ok(), `${r.status()} ${await r.text()}`)
  }

  await page.reload()
  await page.getByText("Premium").first().waitFor()
  await shot(page, "06-portal-after-upgrade")
  const all = (await api(customer, "GET", "/portal/licenses")).licenses
  lic = all.find((l) => l.id === lic.id)
  check("still one licence after the upgrade and a replayed webhook", all.length === 1, `${all.length}`)
  check("same licence moved to Premium", lic?.plan?.name === "Premium", lic?.plan?.name)
  check("licence key unchanged", lic?.license_key === keyBefore)
  check("licence now billed by the Stripe subscription", lic?.stripe_subscription_id === "sub_e2e")

  // ── Same customer tries the free sign-up again ──
  const again = await browser.newContext({ viewport: { width: 1280, height: 900 } })
  const page2 = await again.newPage()
  await signUp(page2, community.checkout_id, CUSTOMER, "07-signup-again")
  check(
    "second sign-up finds the existing licence",
    await page2.getByText("You already have this license").isVisible(),
  )
  const after = (await api(again, "GET", "/portal/licenses")).licenses
  check("no second free licence created", after.length === 1, `${after.length}`)
} catch (e) {
  check("walk-through completed", false, e.stack || String(e))
} finally {
  await browser.close()
  fs.writeFileSync(`${OUT}/results.json`, JSON.stringify(results, null, 2))
}
if (results.some((r) => !r.ok)) process.exit(1)
