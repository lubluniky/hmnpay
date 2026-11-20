import { Skeleton } from "@/components/ui/skeleton"
import { cn } from "@/lib/utils"

const WIDTHS = ["w-40", "w-52", "w-32", "w-44", "w-36", "w-48"]

/** Loading shape of `PaymentsTable`: same header height, row height and column rhythm. */
export function PaymentsTableSkeleton({ rows = 10, className }: { rows?: number; className?: string }) {
  return (
    <div role="status" aria-label="Loading payments" className={cn("divide-y divide-border", className)}>
      <div className="flex h-10 items-center gap-6 px-4 md:px-5">
        <Skeleton className="h-3 w-12" />
        <Skeleton className="ml-auto h-3 w-14" />
      </div>
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="flex h-16 items-center px-4 md:h-11 md:px-5">
          <div className="flex min-w-0 flex-1 items-center gap-3">
            <Skeleton className="h-3.5 w-[72px] shrink-0" />
            <Skeleton className={cn("h-3.5 max-md:hidden", WIDTHS[i % WIDTHS.length])} />
          </div>
          <div className="w-[148px] shrink-0 max-md:hidden">
            <Skeleton className="h-[22px] w-[72px] rounded-full" />
          </div>
          <div className="w-[212px] shrink-0 max-lg:hidden">
            <Skeleton className="h-3.5 w-36" />
          </div>
          <div className="w-[168px] shrink-0 max-xl:hidden">
            <Skeleton className="h-3.5 w-28" />
          </div>
          <div className="w-[132px] shrink-0 max-md:hidden">
            <Skeleton className="h-3.5 w-24" />
          </div>
          <div className="flex w-[148px] shrink-0 justify-end max-md:w-auto">
            <Skeleton className="h-3.5 w-16" />
          </div>
        </div>
      ))}
    </div>
  )
}

/** Loading shape of the payment detail page body (two columns of panels). */
export function PaymentDetailSkeleton() {
  return (
    <div role="status" aria-label="Loading payment" className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_340px]">
      <div className="space-y-6">
        <div className="rounded-xl border border-border bg-card p-5 shadow-card">
          <div className="grid grid-cols-3 gap-6">
            {[0, 1, 2].map((i) => (
              <div key={i} className="space-y-2.5">
                <Skeleton className="h-3 w-16" />
                <Skeleton className="h-6 w-28" />
              </div>
            ))}
          </div>
          <Skeleton className="mt-6 h-5 w-full" />
        </div>
        <div className="space-y-5 rounded-xl border border-border bg-card p-5 shadow-card">
          <Skeleton className="h-3.5 w-20" />
          {[0, 1, 2, 3].map((i) => (
            <div key={i} className="flex items-center gap-3">
              <Skeleton className="size-4 rounded-full" />
              <Skeleton className={cn("h-3.5", WIDTHS[i])} />
              <Skeleton className="ml-auto h-3 w-24" />
            </div>
          ))}
        </div>
      </div>
      <div className="space-y-4 rounded-xl border border-border bg-card p-5 shadow-card">
        <Skeleton className="h-3.5 w-20" />
        {[0, 1, 2, 3, 4].map((i) => (
          <div key={i} className="flex items-center justify-between">
            <Skeleton className="h-3.5 w-20" />
            <Skeleton className="h-3.5 w-28" />
          </div>
        ))}
      </div>
    </div>
  )
}
