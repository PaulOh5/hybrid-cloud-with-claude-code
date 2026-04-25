import type { LabelHTMLAttributes } from "react";

export function Label({
  className = "",
  ...rest
}: LabelHTMLAttributes<HTMLLabelElement>) {
  return (
    <label
      className={`block text-sm font-medium text-zinc-800 dark:text-zinc-200 ${className}`}
      {...rest}
    />
  );
}
