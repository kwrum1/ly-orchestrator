import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const target = process.env.LY_ROUTE_UI_API || "http://127.0.0.1:18090";

export default defineConfig({
  base: "/",
  plugins: [react()],
  server: {
    host: "0.0.0.0",
    port: 5173,
    strictPort: true,
    proxy: {
      "/api": {
        target,
        changeOrigin: false,
        secure: false,
      },
    },
  },
  preview: {
    host: "0.0.0.0",
    port: 4173,
    strictPort: true,
    proxy: {
      "/api": {
        target,
        changeOrigin: false,
        secure: false,
      },
    },
  },
  build: {
    outDir: "dist",
    sourcemap: false,
    target: "es2020",
    cssCodeSplit: false,
    chunkSizeWarningLimit: 1200,
    rollupOptions: {
      output: {
        inlineDynamicImports: true,
        entryFileNames: "app.js",
        chunkFileNames: "assets/[name].js",
        assetFileNames: (assetInfo) => {
          const name = assetInfo.names?.[0] || assetInfo.name || "";
          if (name.endsWith(".css")) return "styles.css";
          return "assets/[name][extname]";
        },
      },
    },
  },
});
