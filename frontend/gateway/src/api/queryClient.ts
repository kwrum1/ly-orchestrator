import { QueryClient } from "@tanstack/react-query";

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: false,
      refetchOnWindowFocus: false,
      staleTime: 5_000,
      gcTime: 5 * 60_000,
    },
    mutations: {
      retry: false,
    },
  },
});
