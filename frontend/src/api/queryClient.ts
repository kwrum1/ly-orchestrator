import { QueryClient } from "@tanstack/react-query";

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: false,
      refetchInterval: 15_000,
      refetchIntervalInBackground: false,
      refetchOnWindowFocus: true,
      staleTime: 5_000,
      gcTime: 5 * 60_000,
    },
    mutations: {
      retry: false,
    },
  },
});
