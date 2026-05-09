import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  reactStrictMode: true,
  // Allow reading from ../docs and ../upstream-readings during build.
  // Next.js's default file-system tracing covers app/, lib/, components/.
};

export default nextConfig;
