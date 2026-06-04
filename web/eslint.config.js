import js from "@eslint/js";
import tseslint from "typescript-eslint";
import reactHooks from "eslint-plugin-react-hooks";
import globals from "globals";

// Correctness-focused lint: real bugs (rules-of-hooks, unsafe patterns), NOT formatting/style. No
// Prettier here — formatting stays advisory. Stylistic/strictness rules are relaxed deliberately.
export default tseslint.config(
  { ignores: ["dist/**", "playwright-report/**", "test-results/**"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ["**/*.{ts,tsx,js}"],
    languageOptions: { globals: { ...globals.browser, ...globals.node } },
    plugins: { "react-hooks": reactHooks },
    rules: {
      // The two correctness rules from react-hooks (manually wired — the plugin's shareable config
      // is still legacy-format in v7).
      "react-hooks/rules-of-hooks": "error",
      "react-hooks/exhaustive-deps": "warn",
      // The thin frame client intentionally uses `any` at the WebSocket boundary — that's a typing
      // choice, not a bug, so don't flag it.
      "@typescript-eslint/no-explicit-any": "off",
      // Unused vars are worth knowing about but shouldn't block; honour the _-prefix opt-out.
      "@typescript-eslint/no-unused-vars": ["warn", { argsIgnorePattern: "^_", varsIgnorePattern: "^_" }],
    },
  },
);
