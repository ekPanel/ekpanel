import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback } from "react";
import { api, ApiError, type User } from "./api";
import i18n from "../i18n";

export const meKey = ["me"] as const;

/** The signed-in user, or null when signed out. */
export function useMe() {
  return useQuery({
    queryKey: meKey,
    queryFn: async () => {
      try {
        return await api.get<User>("/auth/me");
      } catch (e) {
        if (e instanceof ApiError && e.status === 401) return null;
        throw e;
      }
    },
    staleTime: 60_000,
  });
}

export function useSetupStatus() {
  return useQuery({
    queryKey: ["setup-status"],
    queryFn: () => api.get<{ needs_setup: boolean }>("/setup/status"),
    staleTime: Infinity,
  });
}

/** Call after login/setup: stores the user and switches the UI to their saved language. */
export function useSignedIn() {
  const qc = useQueryClient();
  return useCallback(
    (user: User) => {
      qc.setQueryData(meKey, user);
      qc.setQueryData(["setup-status"], { needs_setup: false });
      qc.invalidateQueries({ queryKey: ["branding"] }); // signed in, it follows the account
      if (user.locale && user.locale !== i18n.language) i18n.changeLanguage(user.locale);
    },
    [qc],
  );
}
