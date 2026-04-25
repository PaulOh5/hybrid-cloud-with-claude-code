import type { NextConfig } from "next";

const apiBase = process.env.MAIN_API_URL ?? "http://localhost:8080";

const nextConfig: NextConfig = {
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
};

export default nextConfig;
