import {
  validateTrustInputs,
  type TrustInputEnvironment,
  type TrustInputs,
} from "../../build/trust-inputs";

// Release trust material is stamped into the immutable SPA bundle. Keep access
// lazy so auth-disabled development can import and bootstrap the application.
export function getTrustAnchors(
  env: TrustInputEnvironment = import.meta.env,
): TrustInputs {
  const authRequired = env.PROD === true || Boolean(env.VITE_AS_ORIGIN) || Boolean(env.VITE_AUTH_ENABLED);
  return validateTrustInputs(env, authRequired);
}
