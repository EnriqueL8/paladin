// Copyright © 2024 Kaleido, Inc.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import { Box, Typography } from "@mui/material";
import { t } from "i18next";
import { useContext, useEffect, useState } from "react";
import { IEvent } from "../interfaces";
import { Event } from "./Event";
import { ApplicationContext } from "../Context";

export const Events: React.FC = () => {

  const { lastBlockWithTransactions } = useContext(ApplicationContext);
  const [events, setEvents] = useState<IEvent[]>();

  useEffect(() => {
    let requestPayload = {
      jsonrpc: '2.0',
      id: Date.now(),
      method: 'bidx_queryIndexedEvents',
      params: [{ limit: 100, sort: ['blockNumber DESC', 'transactionIndex DESC'] }]
    };
    fetch('/json-rpc', {
      method: 'post',
      headers: {
        'Accept': 'application/json',
        'Content-Type': 'application/json'
      },
      body: JSON.stringify(requestPayload)
    }).then(async response => {
      setEvents((await response.json()).result);
    });
  }, [lastBlockWithTransactions]);

  return (
    <>
      <Typography align="center" sx={{ fontSize: '24px', fontWeight: 500 }}>{t('events')}</Typography>
      <Box sx={{ height: 'calc(100vh - 163px)', overflow: 'scroll', padding: '20px' }}>
        {events?.map(event =>
          <Event key={`${event.blockNumber}-${event.logIndex}`} event={event} />
        )}
      </Box>
    </>
  );

}