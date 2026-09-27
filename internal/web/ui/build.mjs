import { build } from "esbuild";
import { copyFile, mkdir } from "node:fs/promises";
await build({
  entryPoints: ["app.ts"],
  bundle: true,
  minify: true,
  target: "es2022",
  format: "esm",
  outfile: "../static/app.js",
  legalComments: "linked",
});
await mkdir("../static/vendor", { recursive: true });
// MathML output uses the browser's math fonts; no remote font assets are needed.
await copyFile(
  "node_modules/katex/LICENSE",
  "../static/vendor/katex-LICENSE.txt",
);
