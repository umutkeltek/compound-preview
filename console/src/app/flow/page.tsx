import { redirect } from "next/navigation";

/** Compatibility route for direct rendering environments that skip redirects(). */
export default function FlowCompatibilityPage() {
  redirect("/work?lens=flow");
}
