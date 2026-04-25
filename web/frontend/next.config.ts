import type { NextConfig } from "next";

const apiBase = process.env.MAIN_API_URL ?? "http://localhost:8080";

// Hosts allowed to load Next.js dev resources (HMR websocket etc). Without
// this, accessing the dev server from another machine on the LAN blocks
// /_next/webpack-hmr — client-side JS doesn't hydrate and forms fall back
// to native GET submits.
const allowedDevOrigins = (process.env.NEXT_DEV_ALLOWED_ORIGINS ?? "")
  .split(",")
  .map((s) => s.trim())
  .filter(Boolean);

const nextConfig: NextConfig = {
  // standalone bundles only the production-runtime files (server.js + the
  // minimal node_modules subset) so we can ship a small payload to h20a
  // instead of the entire workspace + node_modules tree.
  output: "standalone",
  // Proxy /api/v1/* to main-api so the browser shares the same origin and
  // the session cookie travels with each request.
  async rewrites() {
    return [
      {
        source: "/api/v1/:path*",
        destination: `${apiBase}/api/v1/:path*`,
      },
    ];
  },
  allowedDevOrigins,
};

export default nextConfig;
