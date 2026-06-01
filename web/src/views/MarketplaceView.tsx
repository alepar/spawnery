import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

export function MarketplaceView(_props: { onSpawn?: (appId: string) => void } = {}) {
  return (
    <div className="grid grid-cols-2 gap-4 p-6 md:grid-cols-3" data-testid="marketplace">
      {[1, 2, 3].map((i) => (
        <Card key={i} className="opacity-60">
          <CardHeader><CardTitle className="text-base">Coming soon</CardTitle></CardHeader>
          <CardContent className="text-sm text-muted-foreground">
            App catalog lands in a later slice.
          </CardContent>
        </Card>
      ))}
    </div>
  );
}
