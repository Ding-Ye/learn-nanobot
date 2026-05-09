import { redirect } from "next/navigation";

// Root: just send users into the Chinese landing. Browsers can hit the EN
// landing directly via /en, or use the language switcher in the sidebar.
export default function Root() {
  redirect("/zh");
}
