export interface IntentTimeMutation {
  issuedAt?: number;
  issuedAtOffsetSeconds?: number;
}

export function resolveIntentIssuedAt(
  mutation: IntentTimeMutation,
  nowSeconds = Math.floor(Date.now() / 1000),
): number {
  return mutation.issuedAt ?? nowSeconds + (mutation.issuedAtOffsetSeconds ?? 0);
}
