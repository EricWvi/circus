import { createContext, useContext } from "react"

import type { DeviceSimulation } from "@/api/mock/backend"

/** The mock's device switches in development builds; null otherwise. */
export const DeviceSimulationContext = createContext<DeviceSimulation | null>(null)

export function useDeviceSimulation(): DeviceSimulation | null {
  return useContext(DeviceSimulationContext)
}
