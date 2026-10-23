import { CheckCircle2, Info, Mail } from "lucide-react"
import { useEffect, useState } from "react"
import { Link, useParams } from "react-router-dom"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { useAuth } from "@/hooks/use-auth"
import { useSiteConfig } from "@/hooks/use-site-config"
import { useI18n } from "@/i18n"
import { type SignupInfo, signup } from "@/lib/api"

// Self sign-up for a free plan: email, then the emailed code, then the
// licence is created and mailed and the visitor is signed in.
export default function SignupPage() {
  const { t } = useI18n()
  const { checkoutId = "" } = useParams()
  const { site_name, logo_url, attribution_text, attribution_url } = useSiteConfig()
  const { refetch } = useAuth()

  const [info, setInfo] = useState<SignupInfo | null>(null)
  const [unavailable, setUnavailable] = useState(false)
  // The form could not be loaded for a reason other than the plan not
  // taking sign-ups (rate limit, server error): worth trying again.
  const [loadFailed, setLoadFailed] = useState(false)
  const [step, setStep] = useState<"email" | "code" | "done">("email")
  const [email, setEmail] = useState("")
  const [code, setCode] = useState("")
  const [consent, setConsent] = useState(false)
  const [result, setResult] = useState<"created" | "existing" | "inactive">("created")
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState("")
  // The plan took all its sign-ups for today; customers who already
  // have a licence sign in instead.
  const [full, setFull] = useState(false)
  const [cooldown, setCooldown] = useState(0)

  useEffect(() => {
    signup
      .info(checkoutId)
      .then(setInfo)
      .catch((err) => {
        if ((err as { code?: string }).code === "SIGNUP_UNAVAILABLE") setUnavailable(true)
        else setLoadFailed(true)
      })
  }, [checkoutId])

  useEffect(() => {
    if (cooldown <= 0) return
    const timer = setTimeout(() => setCooldown(cooldown - 1), 1000)
    return () => clearTimeout(timer)
  }, [cooldown])

  const showError = (err: unknown) => {
    const isFull = (err as { code?: string }).code === "SIGNUP_LIMIT_REACHED"
    setFull(isFull)
    setError(isFull ? t("signup.full") : err instanceof Error ? err.message : t("login.failed"))
  }

  const sendCode = async () => {
    setLoading(true)
    setError("")
    setFull(false)
    try {
      await signup.send(checkoutId, email)
      setStep("code")
      setCooldown(60)
    } catch (err) {
      showError(err)
    } finally {
      setLoading(false)
    }
  }

  const handleSend = async (e: React.FormEvent) => {
    e.preventDefault()
    await sendCode()
  }

  const handleVerify = async (e: React.FormEvent) => {
    e.preventDefault()
    setLoading(true)
    setError("")
    setFull(false)
    try {
      const r = await signup.verify(checkoutId, email, code, consent, info?.consent_text ?? "")
      // The address's one claim may have been revoked or have run out:
      // "you already have it" would send them to a licence that is dead.
      setResult(r.status === "existing" && r.license_usable === false ? "inactive" : r.status)
      setStep("done")
      await refetch()
    } catch (err) {
      showError(err)
    } finally {
      setLoading(false)
    }
  }

  const title = info ? `${info.product_name} ${info.plan_name}`.trim() : site_name

  return (
    <div className="flex flex-col items-center justify-center min-h-screen bg-muted/30">
      <Card className="w-full max-w-sm">
        <CardHeader className="text-center">
          <div className="flex justify-center mb-2">
            <img src={logo_url || "/logo.svg"} alt={site_name} className="h-12 w-12" />
          </div>
          <CardTitle className="text-2xl">{title}</CardTitle>
          {info && step !== "done" && (
            <CardDescription>
              {info.license_type === "trial" && info.trial_days > 0
                ? t("signup.trialSubtitle", { product: info.product_name || site_name, days: info.trial_days })
                : t("signup.subtitle", { product: info.product_name || site_name })}
            </CardDescription>
          )}
        </CardHeader>
        <CardContent className="space-y-3">
          {unavailable && <p className="text-sm text-muted-foreground text-center">{t("signup.unavailable")}</p>}
          {loadFailed && <p className="text-sm text-muted-foreground text-center">{t("signup.loadFailed")}</p>}

          {info && step === "email" && (
            <form onSubmit={handleSend} className="space-y-3">
              <div className="space-y-2">
                <Label>{t("common.email")}</Label>
                <Input
                  type="email"
                  placeholder="you@example.com"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  required
                  autoFocus
                />
              </div>
              {info.consent_text && (
                <label className="flex items-start gap-2 text-sm text-muted-foreground">
                  <input
                    type="checkbox"
                    className="mt-0.5 h-4 w-4"
                    checked={consent}
                    onChange={(e) => setConsent(e.target.checked)}
                  />
                  <span>{info.consent_text}</span>
                </label>
              )}
              {error && <p className="text-sm text-destructive">{error}</p>}
              {full && (
                <Link to="/login" className="block text-center text-sm underline">
                  {t("signup.signIn")}
                </Link>
              )}
              <Button type="submit" className="w-full" disabled={loading}>
                <Mail className="h-4 w-4 mr-2" />
                {loading ? t("login.sendingCode") : t("signup.sendCode")}
              </Button>
            </form>
          )}

          {info && step === "code" && (
            <form onSubmit={handleVerify} className="space-y-3">
              <p className="text-sm text-muted-foreground text-center">{t("login.codeSentTo", { email })}</p>
              <Input
                type="text"
                inputMode="numeric"
                pattern="[0-9]*"
                maxLength={6}
                placeholder="000000"
                value={code}
                onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))}
                required
                autoFocus
                className="text-center text-2xl tracking-[0.5em] font-mono"
              />
              {error && <p className="text-sm text-destructive">{error}</p>}
              {full && (
                <Link to="/login" className="block text-center text-sm underline">
                  {t("signup.signIn")}
                </Link>
              )}
              <Button type="submit" className="w-full" disabled={loading || code.length !== 6}>
                {loading ? t("login.verifying") : t("signup.claim")}
              </Button>
              <div className="flex justify-between text-xs">
                <button
                  type="button"
                  className="text-muted-foreground hover:text-foreground"
                  onClick={() => {
                    setStep("email")
                    setCode("")
                    setError("")
                    setFull(false)
                  }}
                >
                  {t("login.changeEmail")}
                </button>
                <button
                  type="button"
                  className="text-muted-foreground hover:text-foreground disabled:opacity-50"
                  disabled={cooldown > 0 || loading}
                  onClick={sendCode}
                >
                  {cooldown > 0 ? t("login.resendIn", { seconds: String(cooldown) }) : t("login.resendCode")}
                </button>
              </div>
            </form>
          )}

          {step === "done" && (
            <div className="space-y-4 text-center">
              {result === "inactive" ? (
                <Info className="mx-auto h-10 w-10 text-muted-foreground" />
              ) : (
                <CheckCircle2 className="mx-auto h-10 w-10 text-emerald-600" />
              )}
              <p className="font-medium">{t(`signup.${result}Title`)}</p>
              <p className="text-sm text-muted-foreground">{t(`signup.${result}Body`, { email })}</p>
              <Button asChild className="w-full">
                <Link to="/portal">{t("signup.openAccount")}</Link>
              </Button>
            </div>
          )}
        </CardContent>
      </Card>
      {/* Attribution required by AGPL v3 Section 7(b) — see NOTICE */}
      <a
        href={attribution_url}
        target="_blank"
        rel="noopener noreferrer"
        className="mt-4 text-[10px] text-muted-foreground/40 hover:text-muted-foreground transition-colors"
      >
        {attribution_text}
      </a>
    </div>
  )
}
