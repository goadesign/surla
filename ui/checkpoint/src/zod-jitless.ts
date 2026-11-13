// zod-jitless.ts tells the page's validation library, zod, never to compile
// validators from strings. zod otherwise probes whether it may, with
// `new Function("")`, as it builds each object schema; the page's
// Content-Security-Policy forbids evaluating strings, so the probe raised a
// securitypolicyviolation on every load even though zod caught the error.
// app.tsx imports this module first, so the setting holds before any module
// builds a schema; a statement in app.tsx itself would run after its imports.
import { z } from "zod";

z.config({ jitless: true });

// The empty export makes this file an ES module. Without one, the bundler
// treats it as CommonJS (the package's type) and wraps zod and the modules
// that import it in lazy initializers.
export {};
