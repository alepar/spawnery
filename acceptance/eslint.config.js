import js from "@eslint/js";
import tseslint from "typescript-eslint";
import globals from "globals";

// Mirrors web/eslint.config.js minus the react-hooks plugin (this package has no React).
// Correctness-focused: real bugs, not formatting/style.
export default tseslint.config(
  { ignores: ["node_modules/**", "playwright-report/**", "test-results/**"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ["**/*.{ts,js,mjs}"],
    languageOptions: { globals: { ...globals.node } },
    rules: {
      // Driver/fixture code intentionally uses `any` at a few IO boundaries (fetch bodies,
      // subprocess parsing) — that's a typing choice, not a bug.
      "@typescript-eslint/no-explicit-any": "off",
      "@typescript-eslint/no-unused-vars": ["warn", { argsIgnorePattern: "^_", varsIgnorePattern: "^_" }],
    },
  },
);
